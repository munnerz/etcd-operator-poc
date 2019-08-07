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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// EtcdPeerSpec defines the desired state of EtcdPeer
type EtcdPeerSpec struct {
	// ClusterName is the name of the cluster, used as the
	// initial-cluster-token and used as part of the DNS name for each pod.
	ClusterName string `json:"clusterName"`

	// Image is the docker image to use for this EtcdPeer.
	Image string `json:"image"`

	// Bootstrap defines enough bootstrap configuration for this peer to join
	// an existing cluster or form a new one if needed.
	// This configuration is only used when a new cluster is created or when a
	// new peer is being introduced, so it will not be on existing Pod
	// resources if it is updated.
	Bootstrap *BootstrapConfig `json:"bootstrap"`
}

// BootstrapConfig defines how an etcd cluster should be bootstrapped.
type BootstrapConfig struct {
	// Static contains bootstrap configuration that can be used to bootstrap
	// an etcd cluster using 'static' peer configuration.
	Static *StaticBootstrapConfig `json:"static"`
}

// StaticBootstrapConfig is used to configure the 'static' bootstrap method
type StaticBootstrapConfig struct {
	// The initial list of peers in the cluster used when bootstrapping a peer.
	InitialClusterPeers []PeerAddress `json:"initialClusterPeers,omitempty"`
}

// PeerAddress contains connection details for a peer
type PeerAddress struct {
	// Name of the peer
	Name string `json:"name"`

	// Address is the connection string for the peer's peer endpoint.
	Address string `json:"address"`
}

// EtcdPeerStatus defines the observed state of EtcdPeer
type EtcdPeerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// EtcdPeer is the Schema for the etcdpeers API
type EtcdPeer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdPeerSpec   `json:"spec,omitempty"`
	Status EtcdPeerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdPeerList contains a list of EtcdPeer
type EtcdPeerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdPeer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EtcdPeer{}, &EtcdPeerList{})
}
