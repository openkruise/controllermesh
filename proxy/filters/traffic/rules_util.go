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

package traffic

import (
	"strings"

	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	flowcontrolv1beta1 "k8s.io/api/flowcontrol/v1beta1"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
)

func matchesAPIServerRules(ri *apirequest.RequestInfo, rules *ctrlmeshv1alpha1.TrafficAPIServerRules) bool {
	if ri.IsResourceRequest {
		return matchesAResourceRule(ri, rules.ResourceRules)
	}
	return matchesANonResourceRule(ri, rules.NonResourceRules)
}

func matchesAResourceRule(ri *apirequest.RequestInfo, rules []flowcontrolv1beta1.ResourcePolicyRule) bool {
	for _, rr := range rules {
		if matchesResourcePolicyRule(ri, rr) {
			return true
		}
	}
	return false
}

func matchesResourcePolicyRule(ri *apirequest.RequestInfo, policyRule flowcontrolv1beta1.ResourcePolicyRule) bool {
	if !matchPolicyRuleVerb(policyRule.Verbs, ri.Verb) {
		return false
	}
	if !matchPolicyRuleResource(policyRule.Resources, ri.Resource, ri.Subresource) {
		return false
	}
	if !matchPolicyRuleAPIGroup(policyRule.APIGroups, ri.APIGroup) {
		return false
	}
	if len(ri.Namespace) == 0 {
		return policyRule.ClusterScope
	}
	return containsString(ri.Namespace, policyRule.Namespaces, flowcontrolv1beta1.NamespaceEvery)
}

func matchesANonResourceRule(ri *apirequest.RequestInfo, rules []flowcontrolv1beta1.NonResourcePolicyRule) bool {
	for _, rr := range rules {
		if matchesNonResourcePolicyRule(ri, rr) {
			return true
		}
	}
	return false
}

func matchesNonResourcePolicyRule(ri *apirequest.RequestInfo, policyRule flowcontrolv1beta1.NonResourcePolicyRule) bool {
	if !matchPolicyRuleVerb(policyRule.Verbs, ri.Verb) {
		return false
	}
	return matchPolicyRuleNonResourceURL(policyRule.NonResourceURLs, ri.Path)
}

func matchPolicyRuleVerb(policyRuleVerbs []string, requestVerb string) bool {
	return containsString(requestVerb, policyRuleVerbs, flowcontrolv1beta1.VerbAll)
}

func matchPolicyRuleNonResourceURL(policyRuleRequestURLs []string, requestPath string) bool {
	for _, rulePath := range policyRuleRequestURLs {
		if rulePath == flowcontrolv1beta1.NonResourceAll || rulePath == requestPath {
			return true
		}
		rulePrefix := strings.TrimSuffix(rulePath, "*")
		if !strings.HasSuffix(rulePrefix, "/") {
			rulePrefix = rulePrefix + "/"
		}
		if strings.HasPrefix(requestPath, rulePrefix) {
			return true
		}
	}
	return false
}

func matchPolicyRuleAPIGroup(policyRuleAPIGroups []string, requestAPIGroup string) bool {
	return containsString(requestAPIGroup, policyRuleAPIGroups, flowcontrolv1beta1.APIGroupAll)
}

func rsJoin(requestResource, requestSubresource string) string {
	seekString := requestResource
	if requestSubresource != "" {
		seekString = requestResource + "/" + requestSubresource
	}
	return seekString
}

func matchPolicyRuleResource(policyRuleRequestResources []string, requestResource, requestSubresource string) bool {
	return containsString(rsJoin(requestResource, requestSubresource), policyRuleRequestResources, flowcontrolv1beta1.ResourceAll)
}

// containsString returns true if either `x` or `wildcard` is in
// `list`.  The wildcard is not a pattern to match against `x`; rather
// the presence of the wildcard in the list is the caller's way of
// saying that all values of `x` should match the list.  This function
// assumes that if `wildcard` is in `list` then it is the only member
// of the list, which is enforced by validation.
func containsString(x string, list []string, wildcard string) bool {
	if len(list) == 1 && list[0] == wildcard {
		return true
	}
	for _, y := range list {
		if x == y {
			return true
		}
	}
	return false
}
