/*
Copyright 2021 The Kruise Authors.

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

const (
	NameOfManager = "ctrlmesh-manager"
)

// ClusterMetaSpec defines the desired state of ClusterMeta
type ClusterMetaSpec struct {
}

// ClusterMetaStatus defines the observed state of ClusterMeta
type ClusterMetaStatus struct {
	Namespace string               `json:"namespace,omitempty"`
	Endpoints ClusterMetaEndpoints `json:"endpoints,omitempty"`
	Ports     *ClusterMetaPorts    `json:"ports,omitempty"`
}

type ClusterMetaEndpoints []ClusterMetaEndpoint

func (e ClusterMetaEndpoints) Len() int      { return len(e) }
func (e ClusterMetaEndpoints) Swap(i, j int) { e[i], e[j] = e[j], e[i] }
func (e ClusterMetaEndpoints) Less(i, j int) bool {
	return e[i].Name < e[j].Name
}

type ClusterMetaEndpoint struct {
	Name   string `json:"name"`
	PodIP  string `json:"podIP"`
	Leader bool   `json:"leader"`
}

type ClusterMetaPorts struct {
	GrpcLeaderElectionPort    int `json:"grpcLeaderElectionPort,omitempty"`
	GrpcNonLeaderElectionPort int `json:"grpcNonLeaderElectionPort,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ClusterMeta is the Schema for the clustermeta API
type ClusterMeta struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterMetaSpec   `json:"spec,omitempty"`
	Status ClusterMetaStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterMetaList contains a list of ClusterMeta
type ClusterMetaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterMeta `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterMeta{}, &ClusterMetaList{})
}
