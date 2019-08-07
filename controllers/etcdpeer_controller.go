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
	//"github.com/munnerz/etcd-operator/internal/etcd"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"strings"

	"github.com/go-logr/logr"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	etcdv1alpha1 "github.com/munnerz/etcd-operator/api/v1alpha1"
)

// EtcdPeerReconciler reconciles a EtcdPeer object
type EtcdPeerReconciler struct {
	client.Client
	Log logr.Logger
}

// +kubebuilder:rbac:groups=etcd.improbable.io,resources=etcdpeers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=etcd.improbable.io,resources=etcdpeers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *EtcdPeerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("etcdpeer", req.NamespacedName)

	peer := &etcdv1alpha1.EtcdPeer{}
	if err := r.Client.Get(ctx, req.NamespacedName, peer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("Processing EtcdPeer resource")

	log.Info("Checking for existing PersistentVolumeClaim for peer")
	// we don't allow updates the persistentVolumeClaim, making this section of
	// code substantially simpler.
	pvc := &core.PersistentVolumeClaim{}
	// fetch the PVC with the same name as the EtcdPeer
	if err := r.Client.Get(ctx, req.NamespacedName, pvc); err != nil {
		// if no PVC exists, create one and return
		if apierrors.IsNotFound(err) {
			log.Info("Creating new PersistentVolumeClaim for peer")
			return ctrl.Result{}, r.createPVC(ctx, peer)
		}
	}
	log.Info("Found existing PersistentVolumeClaim for peer")

	log.Info("Checking for existing Pod for peer")
	pod := &core.Pod{}
	if err := r.Client.Get(ctx, req.NamespacedName, pod); err != nil {
		// if no Pod exists, create one and return
		if apierrors.IsNotFound(err) {
			log.Info("Creating new Pod for peer")
			return ctrl.Result{}, r.createPod(ctx, log, peer, pvc)
		}
	}
	log.Info("Found existing Pod for peer")

	// TODO: if the pod and pvc already exist, we may also need to add the
	// 'initialized' annotation to the PVC to ensure we don't re-initialize

	return ctrl.Result{}, nil
}

func (r *EtcdPeerReconciler) createPVC(ctx context.Context, peer *etcdv1alpha1.EtcdPeer) error {
	// do not set ownerReferences on this PVC
	pvc := &core.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        peer.Name,
			Namespace:   peer.Namespace,
			Labels:      peer.Labels,
			Annotations: peer.Annotations,
		},
		Spec: core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{
					core.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}

	return r.Client.Create(ctx, pvc)
}

func (r *EtcdPeerReconciler) createPod(ctx context.Context, log logr.Logger, peer *etcdv1alpha1.EtcdPeer, pvc *core.PersistentVolumeClaim) error {
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            peer.Name,
			Namespace:       peer.Namespace,
			Labels:          peer.Labels,
			Annotations:     peer.Annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(peer, etcdv1alpha1.GroupVersion.WithKind("EtcdPeer"))},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{
					Name:  "etcd",
					Image: peer.Spec.Image,
					// TODO: set Resources
					Command: []string{"etcd"},
					Resources: core.ResourceRequirements{
						Requests: core.ResourceList{
							core.ResourceCPU:    resource.MustParse("10m"),
							core.ResourceMemory: resource.MustParse("10Mi"),
						},
					},
					Args: append(computeEtcdArguments(log, peer, pvc), "--data-dir", "/var/lib/etcd"),
					VolumeMounts: []core.VolumeMount{
						{
							Name:      "datadir",
							MountPath: "/var/lib/etcd",
						},
					},
				},
			},
			Hostname:  peer.Name,
			Subdomain: peer.Spec.ClusterName,
			Volumes: []core.Volume{
				{
					Name: "datadir",
					VolumeSource: core.VolumeSource{
						PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc.Name,
						},
					},
				},
			},
		},
	}

	return r.Client.Create(ctx, pod)
}

// computeEtcdArguments determines the arguments that should be passed to the
// etcd peer based on the current state of the peer and PVC resource.
func computeEtcdArguments(log logr.Logger, peer *etcdv1alpha1.EtcdPeer, pvc *core.PersistentVolumeClaim) []string {
	args := []string{"--name", peer.Name}

	currentPeerHostname := buildPeerHostname(peer.Name, peer.Namespace, peer.Spec.ClusterName)
	// TODO: support TLS
	// TODO: support custom peer port
	currentPeerURL := buildPeerURL("http", currentPeerHostname, 2380)
	currentPeerClientURL := buildPeerURL("http", currentPeerHostname, 2379)
	//localPeerClientURL := buildPeerURL("http", "127.0.0.1", 2379)

	args = append(args, "--initial-advertise-peer-urls", currentPeerURL)
	args = append(args, "--listen-peer-urls", "http://0.0.0.0:2380")
	args = append(args, "--listen-client-urls", strings.Join([]string{"http://0.0.0.0:2379"}, ","))
	args = append(args, "--advertise-client-urls", currentPeerClientURL)
	args = append(args, "--initial-cluster", buildInitialClusterString(peer))
	args = append(args, "--initial-cluster-state", determineInitialClusterState(peer, pvc))

	return args
}

func buildPeerURL(scheme, hostname string, port int) string {
	return fmt.Sprintf("%s://%s:%d", scheme, hostname, port)
}

func buildPeerHostname(name, namespace, clusterName string) string {
	return fmt.Sprintf("%s.%s.%s", name, clusterName, namespace)
}

func buildInitialClusterString(peer *etcdv1alpha1.EtcdPeer) string {
	var peers []string
	// TODO: safely check to see if this field is set and return an error
	for _, p := range peer.Spec.Bootstrap.Static.InitialClusterPeers {
		peers = append(peers, fmt.Sprintf("%s=%s", p.Name, p.Address))
	}
	return strings.Join(peers, ",")
}

const (
	// etcdPeerBootstrapAnnotationKey is used to determine whether the cluster
	// is being bootstrapped for the first time.
	etcdPeerBootstrapAnnotationKey = "etcd.improbable.io/bootstrap"

	// etcdPeerIDAnnotationKey is used to identify the hex-encoded ID of a peer
	// when it is being added to an existing cluster.
	// This is used to determine whether peers should be cleaned up or whether
	// they are part of an ongoing scale event
	etcdPeerIDAnnotationKey = "etcd.improbable.io/peer-id"

	// etcdPVCMemberInitializedAnnotationKey is used to determine whether a PVC
	// contains an already initialized data directory.
	// This is used to assist in correctly setting flags when a peer is created
	// based on existing data stored in PVCs.
	etcdPVCMemberInitializedAnnotationKey = "etcd.improbable.io/peer-initialized"
)

func determineInitialClusterState(peer *etcdv1alpha1.EtcdPeer, pvc *core.PersistentVolumeClaim) string {
	if !isBootstrapping(peer) {
		return "existing"
	}
	// it shouldn't actually matter what we set this to, as the cluster has
	// already been bootstrapped here
	if isInitialized(pvc) {
		return "existing"
	}
	// if the cluster *is* bootstrapping and the datadir is empty, this is a
	// new cluster.
	return "new"
}

func isBootstrapping(peer *etcdv1alpha1.EtcdPeer) bool {
	if peer.Annotations == nil {
		return false
	}
	return peer.Annotations[etcdPeerBootstrapAnnotationKey] == "true"
}

func isInitialized(pvc *core.PersistentVolumeClaim) bool {
	if pvc.Annotations == nil {
		return false
	}
	return pvc.Annotations[etcdPVCMemberInitializedAnnotationKey] == "true"
}

func (r *EtcdPeerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// TODO: watch PVC resources
	return ctrl.NewControllerManagedBy(mgr).
		For(&etcdv1alpha1.EtcdPeer{}).
		// Watch for changes to Pod resources that an EtcdPeer owns.
		Owns(&core.Pod{}).
		// We can use a simple EnqueueRequestForObject handler here as the PVC
		// has the same name as the EtcdPeer resource that needs to be enqueued
		Watches(&source.Kind{Type: &core.PersistentVolumeClaim{}}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
