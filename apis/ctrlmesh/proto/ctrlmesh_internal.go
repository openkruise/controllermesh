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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	AllSubsetPublic = "AllSubsetPublic"
	ResourceAll     = "*"
)

func GetLimitsForSubset(subset string, subsetLimits []*SubsetLimitV1) []*MatchLimitRuleV1 {
	for _, subsetLimit := range subsetLimits {
		if subsetLimit.Subset == subset {
			return subsetLimit.Limits
		}
	}
	return nil
}

func ConvertProtoSpecToInternal(protoSpec *ProxySpecV1) *InternalSpec {
	is := &InternalSpec{ProxySpecV1: protoSpec}
	if r := protoSpec.Route; r != nil {
		is.RouteInternal = &InternalRoute{
			Subset:                r.Subset,
			GlobalLimits:          convertProtoMatchLimitRuleToInternal(r.GlobalLimits),
			SubsetPublicResources: r.SubsetPublicResources,
		}
		for _, subsetLimit := range r.SubsetLimits {
			is.RouteInternal.SubsetLimits = append(is.RouteInternal.SubsetLimits, &InternalSubsetLimit{
				Subset: subsetLimit.Subset,
				Limits: convertProtoMatchLimitRuleToInternal(subsetLimit.Limits),
			})
		}
	}

	return is
}

func convertProtoMatchLimitRuleToInternal(limits []*MatchLimitRuleV1) []*InternalMatchLimitRule {
	var internalLimits []*InternalMatchLimitRule
	for _, limit := range limits {
		internalLimits = append(internalLimits, &InternalMatchLimitRule{Namespaces: sets.NewString(limit.Namespaces...), Resources: limit.Resources})
	}
	return internalLimits
}

type InternalSpec struct {
	*ProxySpecV1
	RouteInternal *InternalRoute
}

type InternalRoute struct {
	Subset                string
	GlobalLimits          []*InternalMatchLimitRule
	SubsetLimits          []*InternalSubsetLimit
	SubsetPublicResources []*APIGroupResourceV1
}

type InternalSubsetLimit struct {
	Subset string
	Limits []*InternalMatchLimitRule
}

type InternalMatchLimitRule struct {
	Namespaces sets.String
	// TODO: parse from objectSelector string in protobuf
	// ObjectSelector    *metav1.LabelSelector
	Resources []*APIGroupResourceV1
}

func (ir *InternalRoute) IsDefaultAndEmpty() bool {
	return ir.Subset == "" && len(ir.GlobalLimits) == 0 && len(ir.SubsetLimits) == 0
}

func (ir *InternalRoute) IsNamespaceMatch(ns string, gr schema.GroupResource) bool {
	subset, ok := ir.DetermineNamespaceSubset(ns, gr)
	if !ok {
		return false
	}
	return subset == AllSubsetPublic || subset == ir.Subset
}

func (ir *InternalRoute) DetermineNamespaceSubset(ns string, gr schema.GroupResource) (string, bool) {
	for _, limit := range ir.GlobalLimits {
		if len(limit.Resources) > 0 && !isGRMatchedAPIResources(gr, limit.Resources) {
			continue
		}

		if !limit.Namespaces.Has(ns) {
			return "", false
		}
	}

	isResourceMatchPublic := isGRMatchedAPIResources(gr, ir.SubsetPublicResources)
	var isResourceMatchLimit bool
	for _, subsetLimits := range ir.SubsetLimits {
		for _, limit := range subsetLimits.Limits {
			if len(limit.Resources) > 0 {
				if !isGRMatchedAPIResources(gr, limit.Resources) {
					continue
				}
				isResourceMatchLimit = true
			} else if isResourceMatchPublic {
				continue
			}

			if limit.Namespaces.Has(ns) {
				return subsetLimits.Subset, true
			}
		}
	}
	if isResourceMatchPublic && !isResourceMatchLimit {
		return AllSubsetPublic, true
	}

	return "", true
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
