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

package v1alpha1

import (
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// EtcdClusterSpec defines the desired state of EtcdCluster
type EtcdClusterSpec struct {
	// Number of replicas in the cluster
	// This is a required field for now.
	// In future it will be default to 3.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// Image is the docker image to use for EtcdPeer resources in this cluster
	Image string `json:"image"`

	// Compute Resources required for the etcd container.
	// Updating this field is currently not supported.
	Resources core.ResourceRequirements `json:"resources,omitempty"`

	// Selector must be specified as it'll be set as a label on the EtcdPeer
	// resources the controller creates, and used as a selector for the headless
	// service created for the cluster nodes.
	Selector *metav1.LabelSelector `json:"selector"`

	// TLS configuration for the etcd cluster.
	// If specified, the cluster will use TLS certificates issued by the given
	// cert-manager issuer.
	// +optional
	TLS *EtcdTLS `json:"tls,omitempty"`
}

type EtcdTLS struct {
	// Name of the issuer resource to obtain certificates from
	Name string `json:"name"`

	// Kind of the issuer resource to obtain certificates from
	Kind string `json:"kind"`

	// Group of the issuer resource to obtain certificates from
	Group string `json:"group"`
}

// EtcdClusterStatus defines the observed state of EtcdCluster
type EtcdClusterStatus struct {
	// Members is a list of members in this cluster as returned by the etcd
	// membership API.
	// +optional
	Members []EtcdMemberStatus `json:"members,omitempty"`

	// Selector is the string form of the label selector used to match peers
	// in this cluster.
	// This is used to enable horizontal-pod-autoscaling of EtcdClusters.
	// +optional
	Selector string `json:"selector,omitempty"`

	// Replicas is the number of EtcdPeer resources that exist in the cluster.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
}

type EtcdMemberStatus struct {
	// The human readable name of the member.
	// The name field will not be specified if the member has not been observed
	// in the etcd cluster yet.
	// +optional
	Name string `json:"name,omitempty"`

	// The machine identifier for the etcd peer
	ID string `json:"id"`

	// PeerURLs is a list of peer URL addresses for this peer
	// +optional
	PeerURLs []string `json:"peerURLs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector

// EtcdCluster is the Schema for the etcdclusters API
type EtcdCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdClusterSpec   `json:"spec,omitempty"`
	Status EtcdClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdClusterList contains a list of EtcdCluster
type EtcdClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdCluster{}, &EtcdClusterList{})
}
