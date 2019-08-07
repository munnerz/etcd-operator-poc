/*
Copyright 2019 The Improbable etcd-operator authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/go-logr/logr"
	"go.etcd.io/etcd/clientv3"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	etcdv1alpha1 "github.com/munnerz/etcd-operator/api/v1alpha1"
	"github.com/munnerz/etcd-operator/internal/etcd"
)

// EtcdClusterReconciler reconciles a EtcdCluster object
type EtcdClusterReconciler struct {
	client.Client
	Log      logr.Logger
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=etcd.improbable.io,resources=etcdclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=etcd.improbable.io,resources=etcdclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *EtcdClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("etcdcluster", req.NamespacedName)

	cl := &etcdv1alpha1.EtcdCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cl); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	res, err := r.reconcile(ctx, log, req, cl)
	if updateErr := r.updateStatus(ctx, log, cl); updateErr != nil {
		return res, utilerrors.NewAggregate([]error{err, updateErr})
	}

	return res, err
}

func (r *EtcdClusterReconciler) reconcile(ctx context.Context, log logr.Logger, req ctrl.Request, cluster *etcdv1alpha1.EtcdCluster) (ctrl.Result, error) {
	log.Info("Checking for existing headless Service for cluster")
	pvc := &core.Service{}
	// fetch the Service with the same name as the EtcdCluster
	if err := r.Client.Get(ctx, req.NamespacedName, pvc); err != nil {
		// if no PVC exists, create one and return
		if apierrors.IsNotFound(err) {
			log.Info("Creating new headless Service for cluster")
			return ctrl.Result{}, r.createHeadlessService(ctx, log, cluster)
		}
	}
	log.Info("Found existing headless Service for peer")
	// TODO: validate the Service is up to date

	// get a list of all existing EtcdPeer resources in this cluster
	existingPeers, err := r.getPeers(ctx, log, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	// handle the case where we are creating a cluster for the first time early
	// this is a slightly special case as we assume no scaling of the cluster
	// is required and immediately create all Peer resources so that we are
	// able to proceed with more intelligent actions later.
	// TODO: handle the case where creating at least one peer fails to create better
	//       in some instances, we could end out in an irrecoverable state because
	//       we did not create enough peers to form an actual quorom.
	if len(cluster.Status.Members) == 0 {
		log.Info("Creating all missing cluster peers as no status information exists")
		return ctrl.Result{}, r.createAllMissingPeers(ctx, log, cluster)
	}

	// if any peers are present in the clusters member list but do not exist,
	// we should create them.
	// If this is a scale-down event, they should be properly removed from the
	// member list before they are deleted, so we recreate them to ensure a
	// stable scale-down can occur
	created, err := r.createMissingPeers(ctx, log, cluster, existingPeers)
	if err != nil {
		return ctrl.Result{}, err
	}
	if created {
		log.Info("Created missing peers, waiting until we've observed these peers before taking action")
		return ctrl.Result{}, nil
	}

	// handle members removed from the etcd cluster events
	deleted, err := r.deleteRemovedPeers(ctx, log, cluster, existingPeers)
	if err != nil {
		return ctrl.Result{}, err
	}
	if deleted {
		log.Info("Delete removed peers, waiting until we've observed these peers deletion before taking action")
		return ctrl.Result{}, nil
	}

	peerDiff := int(cluster.Spec.Replicas) - len(existingPeers)
	if peerDiff > 0 {
		return r.handleScaleUp(ctx, log, cluster, existingPeers)
	}
	if peerDiff < 0 {
		return r.handleScaleDown(ctx, log, cluster, existingPeers)
	}

	return ctrl.Result{
		RequeueAfter: time.Second * 1,
	}, nil
}

func (r *EtcdClusterReconciler) handleScaleUp(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster, existingPeers []etcdv1alpha1.EtcdPeer) (ctrl.Result, error) {
	// first check if there is an existing Member without a Name
	//   |-> if there is, check for an EtcdPeer resource with that ID
	//       |-> if one exists, wait for the EtcdPeer resource to finish initialising/warn about potential existing data on disk
	//       |-> if one does not exist:
	//           |-> check if the peer we are going to create would have the correct peer address
	//               |-> if it does, create the EtcdPeer
	//               |-> if it does not, remove the member
	//   |-> if there is not, call AddMember in etcd api and return
	log.Info("Scaling-up etcd cluster")
	memberToCreate, err := determineMemberIdxToCreate(cluster, existingPeers)
	if err != nil {
		log.Error(err, "Failed to determine ID of member to create")
		// TODO: warn user and/or repair this situation
		return ctrl.Result{}, err
	}
	peerName := peerNameInCluster(cluster.Name, memberToCreate)
	peerHostname := buildPeerHostname(peerName, cluster.Namespace, cluster.Name)
	peerURL := buildPeerURL("http", peerHostname, 2380)
	log = log.WithValues("peer_name", peerName, "peer_url", peerURL)

	existingUninitMember, ok := clusterContainsUninitialisedMember(cluster)
	if !ok {
		log.Info("Cluster does not have an uninitialised member registered, registering the new member")
		cl, err := etcd.ClientFor(existingPeers...)
		if err != nil {
			log.Error(err, "Failed to obtain etcd v3 API client")
			return ctrl.Result{}, err
		}
		defer cl.Close()

		r.Recorder.Eventf(cluster, core.EventTypeNormal, "ScaleUp", "Scaling up cluster...")
		member, err := cl.MemberAdd(ctx, []string{peerURL})
		if err != nil {
			log.Error(err, "Failed to add new member")
			return ctrl.Result{}, err
		}
		peerID := fmt.Sprintf("%X", member.Member.ID)
		r.Recorder.Eventf(cluster, core.EventTypeNormal, "AddMember", "Registered new member %q (%s)", peerName, peerID)
		log = log.WithValues("peer_id", peerID)
		log.Info("Registered new member with cluster")
		return ctrl.Result{}, nil
	}

	if len(existingUninitMember.PeerURLs) == 0 {
		log.Info("Unexpected state, no peer URLs registered for member.")
		return ctrl.Result{}, nil
	}
	existingUninitPeerURL := existingUninitMember.PeerURLs[0]
	if len(existingUninitMember.PeerURLs) > 1 {
		log.Info("WARNING - multiple peer URLs found for peer, using the first peer URL... this may have unexpected results.")
	}

	if peerURL != existingUninitPeerURL {
		log.Info("Not scaling up until on-going scale-up event has completed")
		return ctrl.Result{RequeueAfter: time.Second * 1}, nil
	}

	for _, p := range existingPeers {
		if p.Name == peerName {
			// if the hostname for this peer matches the hostname part of the
			// peer URL stored in the members API, we know we have already
			// created the EtcdPeer resource for this member so now we just
			// wait until the peer is properly registered.
			// If this is the case, just return here and wait for the peer to
			// become ready.
			log.Info("Detected existing EtcdPeer resource for member. Waiting for it to initialise...")
			return ctrl.Result{RequeueAfter: time.Second * 1}, nil
		}
	}

	// otherwise if we cannot find an existing peer, create a new one.
	peers, err := buildInitialClusterPeersUsingMembersAPI(ctx, log, cluster, existingPeers)
	if err != nil {
		log.Error(err, "Failed to lookup existing members in etcd cluster, refusing to scale up...")
		return ctrl.Result{}, err
	}
	// the peer name won't yet be available via the membership API as it
	// has not been created yet.
	// Manually fix this on the EtcdPeer resource we create.
	for i, p := range peers {
		if p.Address == peerURL {
			log.Info("Manually setting peer name on peer address for unregistered member")
			peers[i].Name = peerName
			break
		}
	}

	peer := buildEtcdPeerWithName(cluster, peers, peerName, map[string]string{etcdPeerIDAnnotationKey: existingUninitMember.ID})

	log.Info("Creating EtcdPeer resource for member")
	if err := r.Client.Create(ctx, peer); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(cluster, core.EventTypeNormal, "CreatePeer", "Created EtcdPeer resource %q", peer.Name)
	log.Info("Created EtcdPeer resource for member")
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *EtcdClusterReconciler) handleScaleDown(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster, existingPeers []etcdv1alpha1.EtcdPeer) (ctrl.Result, error) {
	log.Info("Scaling-down etcd cluster")
	// TODO: we should verify before scaling down that the cluster is in a safe
	//       state for scaling down, to ensure we don't cause even more issues.
	memberToDelete, err := determineMemberIdxToDelete(cluster, existingPeers)
	if err != nil {
		log.Error(err, "Failed to determine ID of member to create")
		// TODO: warn user and/or repair this situation
		return ctrl.Result{}, err
	}
	peerNameToDelete := peerNameInCluster(cluster.Name, memberToDelete)
	//peerHostname := buildPeerHostname(peerNameToDelete, cluster.Namespace, cluster.Name)
	log = log.WithValues("peer_idx", memberToDelete, "peer_name", peerNameToDelete)
	log.Info("Determining etcd member ID for peer")
	peerIDToRemove := ""
	for _, p := range cluster.Status.Members {
		if p.Name == peerNameToDelete {
			peerIDToRemove = p.ID
		}
	}
	if peerIDToRemove == "" {
		// in this case, we do nothing as the main controller will take care of
		// deleting EtcdPeer resources for deregistered members.
		log.Info("Could not find peer as part of members list. This indicates the member has already been removed.")
		return ctrl.Result{}, nil
	}

	peerID, err := strconv.ParseUint(peerIDToRemove, 16, 64)
	if err != nil {
		log.Error(err, "Unexpected error parsing peer ID hex")
		return ctrl.Result{}, err
	}

	cl, err := etcd.ClientFor(existingPeers...)
	if err != nil {
		log.Error(err, "Error getting etcd API v3 client")
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(cluster, core.EventTypeNormal, "ScaleDown", "Scaling down cluster...")
	_, err = cl.MemberRemove(ctx, peerID)
	if err != nil {
		log.Error(err, "Error removing member from cluster")
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(cluster, core.EventTypeNormal, "RemoveMember", "Deregistered member %q (%s)", peerNameToDelete, peerIDToRemove)
	log.Info("Removed etcd member from cluster")
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func determineMemberIdxToCreate(cluster *etcdv1alpha1.EtcdCluster, existingPeers []etcdv1alpha1.EtcdPeer) (int, error) {
	var peerNames []string
	for _, p := range existingPeers {
		peerNames = append(peerNames, p.Name)
	}
	var peerIDXs []int
	prefixLen := len(cluster.Name) + 1
	for _, p := range peerNames {
		peerIdxStr := p[prefixLen:]
		// TODO: we should probably move this parsing logic out of here and into the call-site of this function
		//       to make it easier to handle this situation gracefully (i.e. warn the user and/or delete the rogue EtcdPeer)
		peerIdx, err := strconv.ParseInt(peerIdxStr, 10, 0)
		if err != nil {
			return 0, fmt.Errorf("error parsing exising peer name")
		}
		peerIDXs = append(peerIDXs, int(peerIdx))
	}
	// sort the peer names
	sort.Ints(peerIDXs)
	for i := 0; i < int(cluster.Spec.Replicas); i++ {
		if i == len(peerIDXs) {
			return i, nil
		}
		if peerIDXs[i] != i {
			return i, nil
		}
	}
	return 0, nil
}

func determineMemberIdxToDelete(cluster *etcdv1alpha1.EtcdCluster, existingPeers []etcdv1alpha1.EtcdPeer) (int, error) {
	var peerNames []string
	for _, p := range existingPeers {
		peerNames = append(peerNames, p.Name)
	}
	var peerIDXs []int
	prefixLen := len(cluster.Name) + 1
	for _, p := range peerNames {
		peerIdxStr := p[prefixLen:]
		// TODO: we should probably move this parsing logic out of here and into the call-site of this function
		//       to make it easier to handle this situation gracefully (i.e. warn the user and/or delete the rogue EtcdPeer)
		peerIdx, err := strconv.ParseInt(peerIdxStr, 10, 0)
		if err != nil {
			return 0, fmt.Errorf("error parsing exising peer name")
		}
		peerIDXs = append(peerIDXs, int(peerIdx))
	}
	// sort the peers
	sort.Ints(peerIDXs)
	return peerIDXs[len(peerIDXs)-1], nil
}

func clusterContainsUninitialisedMember(cluster *etcdv1alpha1.EtcdCluster) (*etcdv1alpha1.EtcdMemberStatus, bool) {
	for _, m := range cluster.Status.Members {
		if m.Name == "" {
			return &m, true
		}
	}
	return nil, false
}

func (r *EtcdClusterReconciler) createMissingPeers(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster, existingPeers []etcdv1alpha1.EtcdPeer) (created bool, err error) {
	existingPeersByNameMap := make(map[string]etcdv1alpha1.EtcdPeer)
	for _, p := range existingPeers {
		existingPeersByNameMap[p.Name] = p
	}

	log.Info("Checking if all registered members have corresponding EtcdPeer resources")
	for _, registeredPeer := range cluster.Status.Members {
		// If the peer is registered but has not been observed by the cluster
		// yet, we don't create it here as it is likely to be in this state due
		// to a scale-up event.
		if registeredPeer.Name == "" {
			continue
		}
		if _, ok := existingPeersByNameMap[registeredPeer.Name]; ok {
			continue
		}

		log := log.WithValues("peer_name", registeredPeer.Name)
		log.Info("Creating new EtcdPeer resource")
		log.Info("Determining initialClusterPeers for new peer resource")
		peers, err := buildInitialClusterPeersUsingMembersAPI(ctx, log, cluster, existingPeers)
		if err != nil {
			log.Error(err, "Failed to lookup existing members in etcd cluster, using cached information...")
			peers = buildInitialClusterPeersUsingMembers(ctx, log, cluster, cluster.Status.Members...)
		}
		// TODO: we don't always need to set the etcdPeerBootstrapAnnotationKey
		//       annotation. This should only be required in the event that we
		//       are creating a peer that is required for the bootstrap process
		peer := buildEtcdPeerWithName(cluster, peers, registeredPeer.Name, map[string]string{etcdPeerBootstrapAnnotationKey: "true"})
		if err := r.Client.Create(ctx, peer); err != nil {
			return false, err
		}
		r.Recorder.Eventf(cluster, core.EventTypeNormal, "CreatePeer", "Created EtcdPeer resource %q", peer.Name)
		log.Info("Created EtcdPeer resource")
		created = true
	}

	return created, nil
}

func (r *EtcdClusterReconciler) deleteRemovedPeers(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster, existingPeers []etcdv1alpha1.EtcdPeer) (deleted bool, err error) {
	if len(cluster.Status.Members) == 0 {
		log.Info("Refusing to delete any EtcdPeer resources as the member list is not available")
		return false, nil
	}

	// when an etcd cluster is bootstrapped for the first time, the initial
	// list of peers is written into the database, including the names and IDs.
	// This means that after the bootstrap phase, if *some* pods are not
	// running, but a quorum of pods *is* running, we won't delete the
	// EtcdPeer resources for the members that have not yet managed to start
	// that were named as part of the bootstrap process.
	//
	// After the bootstrap phase, *every* scale-up event is handled in the
	// recommended two-stage manner, where the member is first introduced
	// via the etcd api before being started.
	// This means we know for certain that the members ID has been recorded
	// in database, so we can be certain that any EtcdPeer resources we have
	// here that do *not* feature in the member list are safe to be deleted.
	//
	// To be sure we don't delete peers that are still starting up for the
	// first time due to cache timing issues, we get an up to date list of
	// members from the etcd cluster.
	// If we are unable to get an up to date list, we don't delete members
	// at all because an unregistered member is harmless anyway and this
	// indicates an issue with the cluster

	// TODO: replcace this with a call to buildInitialClusterPeersUsingMembersAPI
	cl, err := etcd.ClientFor(existingPeers...)
	if err != nil {
		log.Error(err, "Error creating etcd client, refusing to delete any EtcdPeer resources")
		// return false to allow the usual control flow to continue
		return false, nil
	}
	defer cl.Close()

	memberList, err := cl.MemberList(ctx)
	if err != nil {
		log.Error(err, "Error getting member list, refusing to delete any EtcdPeer resources")
		// return false to allow the usual control flow to continue
		return false, nil
	}

	membersByNameMap := make(map[string]*etcdserverpb.Member)
	for _, m := range memberList.Members {
		membersByNameMap[m.Name] = m
	}
	membersByIDMap := make(map[string]*etcdserverpb.Member)
	for _, m := range memberList.Members {
		membersByIDMap[fmt.Sprintf("%X", m.ID)] = m
	}

	log.Info("Checking if any EtcdPeer resources can be cleaned up")
	for _, peer := range existingPeers {
		log := log.WithValues("peer_name", peer.Name)
		_, ok := membersByNameMap[peer.Name]
		if ok {
			log.V(4).Info("Not deleting peer as it is still a member of the cluster")
			continue
		}
		if peer.Annotations != nil {
			peerID := peer.Annotations[etcdPeerIDAnnotationKey]
			if _, ok := membersByIDMap[peerID]; ok {
				log.Info("Not deleting peer as it is part of an ongoing scale-up event")
				continue
			}
		}
		log.Info("Deleting EtcdPeer resource that is no longer a member of the cluster")
		if err := r.Client.Delete(ctx, &peer); err != nil {
			return false, err
		}
		r.Recorder.Eventf(cluster, core.EventTypeNormal, "DeletePeer", "Deleted unregistered EtcdPeer resource %q", peer.Name)
		log.Info("Deleted EtcdPeer resource that is no longer a member of the cluster")
		deleted = true
	}

	return deleted, nil
}

func (r *EtcdClusterReconciler) createHeadlessService(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster) error {
	svc := &core.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    cluster.Labels,
			Annotations: map[string]string{
				"service.alpha.kubernetes.io/tolerate-unready-endpoints": "true",
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cluster, etcdv1alpha1.GroupVersion.WithKind("EtcdCluster"))},
		},
		Spec: core.ServiceSpec{
			ClusterIP:                core.ClusterIPNone,
			Selector:                 cluster.Spec.Selector.MatchLabels,
			PublishNotReadyAddresses: true,
		},
	}
	return r.Client.Create(ctx, svc)
}

func (r *EtcdClusterReconciler) createAllMissingPeers(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster) error {
	initialClusterPeers := buildInitialClusterPeersForEachReplica(cluster)

	for i := 0; i < int(cluster.Spec.Replicas); i++ {
		peer := buildEtcdPeer(cluster, initialClusterPeers, i, map[string]string{etcdPeerBootstrapAnnotationKey: "true"})
		log := log.WithValues("peer_name", peer.Name)
		log.Info("Creating EtcdPeer resource")
		if err := r.Client.Create(ctx, peer); err != nil {
			if apierrors.IsAlreadyExists(err) {
				log.Info("Peer resource already exists")
				continue
			}
			return err
		}
		r.Recorder.Eventf(cluster, core.EventTypeNormal, "CreatePeer", "Created EtcdPeer resource %q", peer.Name)
		log.Info("Created EtcdPeer resource")
	}
	log.Info("Created all EtcdPeer resources for initial cluster")
	return nil
}

func buildEtcdPeer(cluster *etcdv1alpha1.EtcdCluster, peers []etcdv1alpha1.PeerAddress, i int, annotations map[string]string) *etcdv1alpha1.EtcdPeer {
	return buildEtcdPeerWithName(cluster, peers, peerNameInCluster(cluster.Name, i), annotations)
}

func buildEtcdPeerWithName(cluster *etcdv1alpha1.EtcdCluster, peers []etcdv1alpha1.PeerAddress, peerName string, annotations map[string]string) *etcdv1alpha1.EtcdPeer {
	peer := &etcdv1alpha1.EtcdPeer{
		ObjectMeta: metav1.ObjectMeta{
			Name:            peerName,
			Namespace:       cluster.Namespace,
			Labels:          cluster.Spec.Selector.MatchLabels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(cluster, etcdv1alpha1.GroupVersion.WithKind("EtcdCluster"))},
		},
		Spec: etcdv1alpha1.EtcdPeerSpec{
			ClusterName: cluster.Name,
			Image:       cluster.Spec.Image,
			Bootstrap: &etcdv1alpha1.BootstrapConfig{
				Static: &etcdv1alpha1.StaticBootstrapConfig{
					InitialClusterPeers: peers,
				},
			},
		},
	}
	return peer
}

// buildInitialClusterPeersUsingMembersAPI will build a list of current member
// peer addresses based on what the etcd returns as part of a MemberList query
func buildInitialClusterPeersUsingMembersAPI(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster, existingPeers []etcdv1alpha1.EtcdPeer) ([]etcdv1alpha1.PeerAddress, error) {
	memberList, err := buildMemberList(ctx, log, existingPeers)
	if err != nil {
		return nil, err
	}

	return buildInitialClusterPeersUsingMembers(ctx, log, cluster, memberList...), nil
}

func buildInitialClusterPeersUsingMembers(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster, members ...etcdv1alpha1.EtcdMemberStatus) []etcdv1alpha1.PeerAddress {
	var peers []etcdv1alpha1.PeerAddress
	for _, m := range members {
		peerURLs := strings.Join(m.PeerURLs, ",")
		peers = append(peers, etcdv1alpha1.PeerAddress{
			Name:    m.Name,
			Address: peerURLs,
		})
	}
	return peers
}

func buildInitialClusterPeersForEachReplica(cluster *etcdv1alpha1.EtcdCluster) []etcdv1alpha1.PeerAddress {
	var peers []etcdv1alpha1.PeerAddress
	for i := 0; i < int(cluster.Spec.Replicas); i++ {
		peerName := peerNameInCluster(cluster.Name, i)
		peerHostname := buildPeerHostname(peerName, cluster.Namespace, cluster.Name)
		peers = append(peers, etcdv1alpha1.PeerAddress{
			Name:    peerName,
			Address: buildPeerURL("http", peerHostname, 2380),
		})
	}
	return peers
}

func peerNameInCluster(clusterName string, i int) string {
	return fmt.Sprintf("%s-%d", clusterName, i)
}

func (r *EtcdClusterReconciler) getPeers(ctx context.Context, log logr.Logger, cl *etcdv1alpha1.EtcdCluster) ([]etcdv1alpha1.EtcdPeer, error) {
	peerSelector, err := metav1.LabelSelectorAsSelector(cl.Spec.Selector)
	if err != nil {
		return nil, err
	}

	peerList := &etcdv1alpha1.EtcdPeerList{}
	if err := r.Client.List(ctx, peerList, labelSelectorListOptionsFunc(peerSelector)); err != nil {
		return nil, err
	}

	return peerList.Items, nil
}

func labelSelectorListOptionsFunc(sel labels.Selector) func(opts *client.ListOptions) {
	return func(opts *client.ListOptions) {
		opts.LabelSelector = sel
	}
}

func (r *EtcdClusterReconciler) updateStatus(ctx context.Context, log logr.Logger, cluster *etcdv1alpha1.EtcdCluster) error {
	log.Info("Updating EtcdCluster status")

	log.Info("Getting list of existing peers")
	// get a list of all existing EtcdPeer resources in this cluster
	existingPeers, err := r.getPeers(ctx, log, cluster)
	if err != nil {
		return err
	}
	log.Info("Found existing EtcdPeer resources", "count", len(existingPeers))

	// TODO: handle the case where the etcdcluster is unavailable better
	cluster.Status.Members, err = buildMemberList(ctx, log, existingPeers)
	if err != nil {
		return err
	}

	return r.Client.Status().Update(ctx, cluster)
}

func buildMemberList(ctx context.Context, log logr.Logger, existingPeers []etcdv1alpha1.EtcdPeer) ([]etcdv1alpha1.EtcdMemberStatus, error) {
	log.Info("Attempting to obtain etcdv3 API client for peers")
	cl, err := etcd.ClientFor(existingPeers...)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	log.Info("Obtained etcdv3 API client", "endpoints", cl.Endpoints())

	log.Info("Obtaining etcdv3 Cluster API client")
	clusterClient := clientv3.NewCluster(cl)

	log.Info("Querying cluster for member list")
	memberListResp, err := clusterClient.MemberList(ctx)
	if err != nil {
		log.Error(err, "error obtaining etcd member list")
		return nil, err
	}
	log.Info("Fetched member list from etcd cluster")

	members := memberListResponseToStatusList(*memberListResp)
	membersByName := make(map[string]*etcdv1alpha1.EtcdMemberStatus)
	for _, m := range members {
		membersByName[m.Name] = &m
	}
	membersByID := make(map[string]*etcdv1alpha1.EtcdMemberStatus)
	for _, m := range members {
		membersByID[m.ID] = &m
	}

	for _, peer := range existingPeers {
		// if the member already appears in the member list looked up from the
		// etcd cluster, don't do anymore processing as all information should
		// be pulled direct from the cluster if possible
		if _, ok := membersByName[peer.Name]; ok {
			continue
		}

		if peer.Annotations == nil || peer.Annotations[etcdPeerIDAnnotationKey] == "" {
			// if the peer does not have an ID set then we cannot inject a
			// synthetic entry for it, so skip it.
			continue
		}

		peerID := peer.Annotations[etcdPeerIDAnnotationKey]
		// if this member's ID already features, the member has been registered
		// into the cluster but has not started yet, so inject a synthetic
		// member status that sets 'initialised' to false.
		if m, ok := membersByID[peerID]; ok {
			m.Name = peer.Name
			continue
		}

		// otherwise, the member is not registered with the cluster and a peer
		// resource exists, which means it needs to be deleted as it isn't a
		// member of the cluster.
		// do nothing in this case as the member should not be connected to.
	}

	return members, nil
}

func memberListResponseToStatusList(response clientv3.MemberListResponse) []etcdv1alpha1.EtcdMemberStatus {
	var members []etcdv1alpha1.EtcdMemberStatus
	for _, m := range response.Members {
		members = append(members, etcdv1alpha1.EtcdMemberStatus{
			Name:     m.Name,
			ID:       fmt.Sprintf("%X", m.ID),
			PeerURLs: m.PeerURLs,
		})
	}
	return members
}

func (r *EtcdClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&etcdv1alpha1.EtcdCluster{}).
		Owns(&etcdv1alpha1.EtcdPeer{}).
		Owns(&core.Service{}).
		Complete(r)
}
