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
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"

	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
)

type InternalRoute struct {
	Subset string

	GlobalLimits            []ctrlmeshv1alpha1.MatchLimitSelector
	SubRules                []ctrlmeshv1alpha1.VirtualAppRouteSubRule
	Subsets                 []ctrlmeshv1alpha1.VirtualAppSubset
	GlobalExcludeNamespaces sets.String
	SensSubsetNamespaces    []InternalSensSubsetNamespaces
}

type InternalSensSubsetNamespaces struct {
	Name       string
	Namespaces sets.String
}

func (ir *InternalRoute) DecodeFrom(route *Route) error {
	if ir == nil {
		return fmt.Errorf("can not parse into nil")
	} else if route == nil {
		return nil
	}
	ir.Subset = route.Subset
	if len(route.GlobalLimits) > 0 {
		if err := json.Unmarshal([]byte(route.GlobalLimits), &ir.GlobalLimits); err != nil {
			return err
		}
	}
	if len(route.SubRules) > 0 {
		if err := json.Unmarshal([]byte(route.SubRules), &ir.SubRules); err != nil {
			return err
		}
	}
	if len(route.Subsets) > 0 {
		if err := json.Unmarshal([]byte(route.Subsets), &ir.Subsets); err != nil {
			return err
		}
	}
	ir.SensSubsetNamespaces = nil
	for _, s := range route.SensSubsetNamespaces {
		ir.SensSubsetNamespaces = append(ir.SensSubsetNamespaces, InternalSensSubsetNamespaces{Name: s.Name, Namespaces: sets.NewString(s.Namespaces...)})
	}
	if len(route.GlobalExcludeNamespaces) > 0 {
		ir.GlobalExcludeNamespaces = sets.NewString(route.GlobalExcludeNamespaces...)
	}
	return nil
}

func (ir *InternalRoute) Encode() *Route {
	if ir == nil {
		return nil
	}
	if ir.GlobalLimits == nil {
		ir.GlobalLimits = make([]ctrlmeshv1alpha1.MatchLimitSelector, 0)
	}
	if ir.SubRules == nil {
		ir.SubRules = make([]ctrlmeshv1alpha1.VirtualAppRouteSubRule, 0)
	}
	if ir.Subsets == nil {
		ir.Subsets = make([]ctrlmeshv1alpha1.VirtualAppSubset, 0)
	}
	var sensSubsetNamespaces []*SensSubsetNamespaces
	for _, s := range ir.SensSubsetNamespaces {
		sensSubsetNamespaces = append(sensSubsetNamespaces, &SensSubsetNamespaces{Name: s.Name, Namespaces: s.Namespaces.List()})
	}
	return &Route{
		Subset:                  ir.Subset,
		GlobalLimits:            dumpJSON(ir.GlobalLimits),
		SubRules:                dumpJSON(ir.SubRules),
		Subsets:                 dumpJSON(ir.Subsets),
		SensSubsetNamespaces:    sensSubsetNamespaces,
		GlobalExcludeNamespaces: ir.GlobalExcludeNamespaces.List(),
	}
}

func (ir *InternalRoute) IsDefaultEmpty() bool {
	return ir.Subset == "" && ir.GlobalExcludeNamespaces.Len() == 0 && len(ir.SensSubsetNamespaces) == 0
}

func (ir *InternalRoute) IsNamespaceMatch(ns string) bool {
	subset, ok := ir.DetermineNamespaceSubset(ns)
	if !ok {
		return false
	}
	return subset == ir.Subset
}

func (ir *InternalRoute) DetermineNamespaceSubset(ns string) (string, bool) {
	if ir.GlobalExcludeNamespaces.Has(ns) {
		return "", false
	}
	// currently cluster scope resources can only handled by the default instance
	if ns == "" {
		return "", true
	}
	for _, s := range ir.SensSubsetNamespaces {
		if s.Namespaces.Has(ns) {
			return s.Name, true
		}
	}
	return "", true
}

func dumpJSON(o interface{}) string {
	j, _ := json.Marshal(o)
	return string(j)
}
