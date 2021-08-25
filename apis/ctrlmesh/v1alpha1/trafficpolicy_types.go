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
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	flowcontrolv1beta1 "k8s.io/api/flowcontrol/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TrafficPolicySpec defines the desired state of TrafficPolicy
type TrafficPolicySpec struct {
	TargetVirtualApps []TrafficTargetVirtualApp `json:"targetVirtualApps"`
	CircuitBreaking   *TrafficCircuitBreaking   `json:"circuitBreaking,omitempty"`
	RateLimiting      *TrafficRateLimiting      `json:"rateLimiting,omitempty"`
}

// TrafficTargetVirtualApp is the target VirtualApp and its optional specific subsets.
type TrafficTargetVirtualApp struct {
	Name            string   `json:"name"`
	SpecificSubsets []string `json:"specificSubsets,omitempty"`
}

// TrafficCircuitBreaking defines policies that ctrlmesh-proxy should intercept the requests.
type TrafficCircuitBreaking struct {
	APIServer *TrafficAPIServerRules `json:"apiServer,omitempty"`
	Webhook   *TrafficWebhookRules   `json:"webhook,omitempty"`
}

type TrafficRateLimiting struct {
	RatePolicies []TrafficRateLimitingPolicy `json:"ratePolicies,omitempty"`
}

type TrafficRateLimitingPolicy struct {
	Rules TrafficAPIServerRules `json:"rules"`

	MaxInFlight        *int32                                 `json:"maxInFlight,omitempty"`
	Bucket             *TrafficRateLimitingBucket             `json:"bucket,omitempty"`
	ExponentialBackoff *TrafficRateLimitingExponentialBackoff `json:"exponentialBackoff,omitempty"`
}

type TrafficRateLimitingBucket struct {
	QPS   int32 `json:"qps"`
	Burst int32 `json:"burst"`
}

type TrafficRateLimitingExponentialBackoff struct {
	BaseDelayInMillisecond   int32 `json:"baseDelayInMillisecond"`
	MaxDelayInMillisecond    int32 `json:"maxDelayInMillisecond"`
	ContinuouslyFailureTimes int32 `json:"continuouslyFailureTimes,omitempty"`
}

// TrafficAPIServerRules contains rules for apiserver requests.
type TrafficAPIServerRules struct {
	// `resourceRules` is a slice of ResourcePolicyRules that identify matching requests according to their verb and the
	// target resource.
	// At least one of `resourceRules` and `nonResourceRules` has to be non-empty.
	ResourceRules []flowcontrolv1beta1.ResourcePolicyRule `json:"resourceRules,omitempty"`
	// `nonResourceRules` is a list of NonResourcePolicyRules that identify matching requests according to their verb
	// and the target non-resource URL.
	NonResourceRules []flowcontrolv1beta1.NonResourcePolicyRule `json:"nonResourceRules,omitempty"`
}

// TrafficWebhookRules contains rules for webhook requests.
type TrafficWebhookRules struct {
	// Rules describes what operations on what resources/subresources should be intercepted.
	AdmissionRules []admissionregistrationv1.RuleWithOperations `json:"admissionRules,omitempty"`
}

// TrafficPolicyStatus defines the observed state of TrafficPolicy
type TrafficPolicyStatus struct {
}

// +genclient
// +k8s:openapi-gen=true
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// TrafficPolicy is the Schema for the trafficpolicies API
type TrafficPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrafficPolicySpec   `json:"spec,omitempty"`
	Status TrafficPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// TrafficPolicyList contains a list of TrafficPolicy
type TrafficPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrafficPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrafficPolicy{}, &TrafficPolicyList{})
}
