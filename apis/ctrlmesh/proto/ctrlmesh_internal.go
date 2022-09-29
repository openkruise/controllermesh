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

package proto

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/openkruise/controllermesh/util"
)

const (
	AllSubsetPublic = "AllSubsetPublic"
	ResourceAll     = "*"
)

type ResourceRequest struct {
	GR schema.GroupResource `json:"GR,omitempty"`

	ObjectSelector  *metav1.LabelSelector `json:"objectSelector,omitempty"`
	NamespacePassed sets.String           `json:"namespacePassed,omitempty"`
	NamespaceDenied sets.String           `json:"namespaceDenied,omitempty"`
	UserAgentPassed *string               `json:"userAgentPassed,omitempty"`
	UserAgentDenied *string               `json:"userAgentDenied,omitempty"`
}

func ConvertProtoSpecToInternal(protoSpec *ProxySpecV1) *InternalSpec {
	is := &InternalSpec{ProxySpecV1: protoSpec}
	if r := protoSpec.Route; r != nil {
		is.routeInternal = &internalRoute{
			subset:                      r.Subset,
			globalLimits:                convertProtoMatchLimitRuleToInternal(r.GlobalLimits),
			subsetPublicResources:       r.SubsetPublicResources,
			subsetDefaultOnlyUserAgents: sets.NewString(r.SubsetDefaultOnlyUserAgents...),
		}
		for _, subsetLimit := range r.SubsetLimits {
			is.routeInternal.subsetLimits = append(is.routeInternal.subsetLimits, &internalSubsetLimit{
				subset: subsetLimit.Subset,
				limits: convertProtoMatchLimitRuleToInternal(subsetLimit.Limits),
			})
		}
	}

	return is
}

func convertProtoMatchLimitRuleToInternal(limits []*MatchLimitRuleV1) []*internalMatchLimitRule {
	var internalLimits []*internalMatchLimitRule
	for _, limit := range limits {

		if len(limit.ObjectSelector) > 0 {
			selector := &metav1.LabelSelector{}
			if err := json.Unmarshal([]byte(limit.ObjectSelector), selector); err != nil {
				klog.Errorf("fail to unmarshal ObjectSelector %s", limit.ObjectSelector)
			}
			internalLimits = append(internalLimits, &internalMatchLimitRule{
				resources:      limit.Resources,
				objectSelector: selector,
			})
		} else {
			internalLimits = append(internalLimits, &internalMatchLimitRule{
				namespaces: sets.NewString(limit.Namespaces...),
				resources:  limit.Resources,
			})
		}
	}
	return internalLimits
}

type InternalSpec struct {
	*ProxySpecV1
	routeInternal *internalRoute
}

func (is *InternalSpec) IsUserAgentMatch(userAgent string) bool {
	if is.routeInternal.subsetDefaultOnlyUserAgents.Has(userAgent) {
		// Only for default subset
		return is.routeInternal.subset == ""
	}
	return true
}

func (is *InternalSpec) IsNamespaceMatch(ns string, gr schema.GroupResource) bool {
	subset, ok := is.routeInternal.determineNamespaceSubset(ns, gr)
	if !ok {
		return false
	}
	return subset == AllSubsetPublic || subset == is.routeInternal.subset
}

func (is *InternalSpec) GetObjectSelector(gr schema.GroupResource) (sel *metav1.LabelSelector) {
	return is.routeInternal.getObjectSelector(gr)
}

func (is *InternalSpec) GetMatchedSubsetEndpoint(ns string, gr schema.GroupResource) (ignore bool, self bool, hosts []string) {
	subset, ok := is.routeInternal.determineNamespaceSubset(ns, gr)
	if !ok {
		return true, false, nil
	}
	if subset == AllSubsetPublic || subset == is.routeInternal.subset {
		return false, true, nil
	}

	// TODO: how to route webhook request by object selector?

	for _, e := range is.Endpoints {
		if e.Subset == subset {
			hosts = append(hosts, e.Ip)
		}
	}
	return false, false, hosts
}

type internalRoute struct {
	subset                      string
	globalLimits                []*internalMatchLimitRule
	subsetLimits                []*internalSubsetLimit
	subsetPublicResources       []*APIGroupResourceV1
	subsetDefaultOnlyUserAgents sets.String
}

type internalSubsetLimit struct {
	subset string
	limits []*internalMatchLimitRule
}

type internalMatchLimitRule struct {
	namespaces     sets.String
	objectSelector *metav1.LabelSelector
	resources      []*APIGroupResourceV1
}

func (ir *internalRoute) determineNamespaceSubset(ns string, gr schema.GroupResource) (string, bool) {
	// Resources of ClusterScope can only be handled by default subset
	if ns == "" {
		return "", true
	}

	for _, limit := range ir.globalLimits {
		if len(limit.resources) > 0 && !isGRMatchedAPIResources(gr, limit.resources) {
			continue
		} else if limit.objectSelector != nil {
			continue
		}

		if !limit.namespaces.Has(ns) {
			return "", false
		}
	}

	isResourceMatchPublic := isGRMatchedAPIResources(gr, ir.subsetPublicResources)
	var isResourceMatchLimit bool
	for _, subsetLimits := range ir.subsetLimits {
		for _, limit := range subsetLimits.limits {
			if limit.objectSelector != nil {
				continue
			}
			if len(limit.resources) > 0 {
				if !isGRMatchedAPIResources(gr, limit.resources) {
					continue
				}
				isResourceMatchLimit = true
			} else if isResourceMatchPublic {
				continue
			}

			if limit.namespaces.Has(ns) {
				return subsetLimits.subset, true
			}
		}
	}
	if isResourceMatchPublic && !isResourceMatchLimit {
		return AllSubsetPublic, true
	}

	// No subset matched, should ignore it instead of using default subset
	return "", false
}

func (ir *internalRoute) getObjectSelector(gr schema.GroupResource) (sel *metav1.LabelSelector) {
	for _, limit := range ir.globalLimits {
		if limit.objectSelector == nil {
			continue
		}

		if len(limit.resources) > 0 && !isGRMatchedAPIResources(gr, limit.resources) {
			continue
		}
		sel = util.MergeLabelSelector(sel, limit.objectSelector)
	}

	for _, subsetLimits := range ir.subsetLimits {
		isSelfSubset := subsetLimits.subset == ir.subset

		for _, limit := range subsetLimits.limits {
			if limit.objectSelector == nil {
				continue
			}
			if len(limit.resources) > 0 && !isGRMatchedAPIResources(gr, limit.resources) {
				continue
			}
			if !isSelfSubset {
				sel = util.MergeLabelSelector(sel, util.NegateLabelSelector(limit.objectSelector))
			} else {
				sel = util.MergeLabelSelector(sel, limit.objectSelector)
			}
		}

		if isSelfSubset {
			break
		}
	}
	return
}

func isGRMatchedAPIResources(gr schema.GroupResource, resources []*APIGroupResourceV1) bool {
	for _, r := range resources {
		if containsString(gr.Group, r.ApiGroups, ResourceAll) && containsString(gr.Resource, r.Resources, ResourceAll) {
			return true
		}
	}
	return false
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
