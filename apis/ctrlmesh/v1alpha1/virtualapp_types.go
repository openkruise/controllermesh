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
	VirtualAppInjectedKey = "ctrlmesh.kruise.io/virtual-app-injected"
)

// VirtualAppSpec defines the desired state of VirtualApp
type VirtualAppSpec struct {
	// Selector is a label query over pods of this application.
	Selector *metav1.LabelSelector `json:"selector"`
	// Configuration defines the configuration of controller and webhook in this application.
	Configuration *VirtualAppConfiguration `json:"configuration,omitempty"`
	// Route defines the route of this application including global and sub rules.
	Route *VirtualAppRoute `json:"route,omitempty"`
	// Subsets defines the subsets for this application.
	Subsets []VirtualAppSubset `json:"subsets,omitempty"`
}

// VirtualAppConfiguration defines the configuration of controller or webhook of this application.
type VirtualAppConfiguration struct {
	Controller *VirtualAppControllerConfiguration `json:"controller,omitempty"`
	Webhook    *VirtualAppWebhookConfiguration    `json:"webhook,omitempty"`
}

// VirtualAppRestConfigOverrides defines overrides to the application's rest config.
type VirtualAppRestConfigOverrides struct {
	// UserAgentOrPrefix can override the UserAgent of application.
	// If it ends with '/', we consider it as prefix and will be add to the front of original UserAgent.
	// Otherwise it will replace the original UserAgent.
	UserAgentOrPrefix *string `json:"userAgentOrPrefix,omitempty"`
}

// VirtualAppControllerConfiguration defines the configuration of controller in this application.
type VirtualAppControllerConfiguration struct {
	LeaderElectionName string `json:"leaderElectionName"`
}

// VirtualAppWebhookConfiguration defines the configuration of webhook in this application.
type VirtualAppWebhookConfiguration struct {
	CertDir string `json:"certDir"`
	Port    int    `json:"port"`
}

// VirtualAppRoute defines the route of this application including global and sub-rules.
type VirtualAppRoute struct {
	GlobalLimits          []MatchLimitSelector     `json:"globalLimits,omitempty"`
	SubRules              []VirtualAppRouteSubRule `json:"subRules,omitempty"`
	SubsetPublicResources []APIGroupResource       `json:"subsetPublicResources,omitempty"`
}

type VirtualAppRouteSubRule struct {
	Name  string               `json:"name"`
	Match []MatchLimitSelector `json:"match"`
}

type MatchLimitSelector struct {
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	ObjectSelector    *metav1.LabelSelector `json:"objectSelector,omitempty"`
	NamespaceRegex    *string               `json:"namespaceRegex,omitempty"`
	Resources         []APIGroupResource    `json:"resources,omitempty"`
}

type APIGroupResource struct {
	// APIGroups is the API groups the resources belong to. '*' is all groups.
	// If '*' is present, the length of the slice must be one.
	APIGroups []string `json:"apiGroups"`
	// Resources is a list of resources this rule applies to.
	//
	// For example:
	// 'pods' means pods.
	// 'pods/log' means the log subresource of pods.
	// '*' means all resources, but not subresources.
	// 'pods/*' means all subresources of pods.
	// '*/scale' means all scale subresources.
	// '*/*' means all resources and their subresources.
	//
	// If wildcard is present, the validation rule will ensure resources do not
	// overlap with each other.
	//
	// Depending on the enclosing object, subresources might not be allowed.
	Resources []string `json:"resources"`
}

type VirtualAppSubset struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	RouteRules []string          `json:"routeRules"`
}

// VirtualAppStatus defines the observed state of VirtualApp
type VirtualAppStatus struct {
	// TODO: design the report fields
}

// +genclient
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vapp

// VirtualApp is the Schema for the virtualapps API
type VirtualApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualAppSpec   `json:"spec,omitempty"`
	Status VirtualAppStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// VirtualAppList contains a list of VirtualApp
type VirtualAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualApp `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VirtualApp{}, &VirtualAppList{})
}
