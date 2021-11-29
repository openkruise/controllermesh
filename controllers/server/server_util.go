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

package server

import (
	"regexp"
	"sort"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/util"
)

func determinePodSubset(vApp *ctrlmeshv1alpha1.VirtualApp, pod *v1.Pod) string {
	for i := range vApp.Spec.Subsets {
		subset := &vApp.Spec.Subsets[i]
		matched := true
		for k, v := range subset.Labels {
			if pod.Labels[k] != v {
				matched = false
				break
			}
		}
		if matched {
			return subset.Name
		}
	}
	return ""
}

func generateProtoRoute(vApp *ctrlmeshv1alpha1.VirtualApp, namespaces []*v1.Namespace) *ctrlmeshproto.RouteV1 {
	protoRoute := &ctrlmeshproto.RouteV1{}

	// global limits
	if vApp.Spec.Route != nil {
		protoRoute.GlobalLimits, _ = generateMatchLimitRules(vApp.Spec.Route.GlobalLimits, namespaces)
		protoRoute.SubsetPublicResources = generateAPIResources(vApp.Spec.Route.SubsetPublicResources)
	}

	// build rules map
	rulesMap := make(map[string][]ctrlmeshv1alpha1.MatchLimitSelector)
	if vApp.Spec.Route != nil {
		for i := range vApp.Spec.Route.SubRules {
			r := &vApp.Spec.Route.SubRules[i]
			rulesMap[r.Name] = r.Match
		}
	}

	// subsets limits
	var allExcludeLimits []*ctrlmeshproto.MatchLimitRuleV1
	for i := range vApp.Spec.Subsets {
		subset := &vApp.Spec.Subsets[i]
		var subsetLimits []ctrlmeshv1alpha1.MatchLimitSelector
		for _, ruleName := range subset.RouteRules {
			subsetLimits = append(subsetLimits, rulesMap[ruleName]...)
		}
		includeLimits, excludeLimits := generateMatchLimitRules(subsetLimits, namespaces)
		allExcludeLimits = append(allExcludeLimits, excludeLimits...)
		protoRoute.SubsetLimits = append(protoRoute.SubsetLimits, &ctrlmeshproto.SubsetLimitV1{
			Subset: subset.Name,
			Limits: includeLimits,
		})
	}

	// add default subset
	protoRoute.SubsetLimits = append(protoRoute.SubsetLimits, &ctrlmeshproto.SubsetLimitV1{
		Subset: "",
		Limits: allExcludeLimits,
	})

	return protoRoute
}

func generateMatchLimitRules(limits []ctrlmeshv1alpha1.MatchLimitSelector, namespaces []*v1.Namespace) (includeRules, excludeRules []*ctrlmeshproto.MatchLimitRuleV1) {
	if len(limits) == 0 {
		return nil, nil
	}
	for _, ms := range limits {
		resources := generateAPIResources(ms.Resources)
		switch {
		case ms.NamespaceSelector != nil, ms.NamespaceRegex != nil:
			var matchedNamespaces []string
			var unmatchedNamespaces []string
			for _, ns := range namespaces {
				if isNamespaceMatched(ms, ns) {
					matchedNamespaces = append(matchedNamespaces, ns.Name)
				} else {
					unmatchedNamespaces = append(unmatchedNamespaces, ns.Name)
				}
			}
			includeRules = append(includeRules, &ctrlmeshproto.MatchLimitRuleV1{Namespaces: matchedNamespaces, Resources: resources})
			excludeRules = append(excludeRules, &ctrlmeshproto.MatchLimitRuleV1{Namespaces: unmatchedNamespaces, Resources: resources})
			// TODO: object selector
		}
	}
	return
}

func generateAPIResources(resources []ctrlmeshv1alpha1.APIGroupResource) []*ctrlmeshproto.APIGroupResourceV1 {
	var ret []*ctrlmeshproto.APIGroupResourceV1
	for _, resource := range resources {
		ret = append(ret, &ctrlmeshproto.APIGroupResourceV1{ApiGroups: resource.APIGroups, Resources: resource.Resources})
	}
	return ret
}

func generateProtoEndpoints(vApp *ctrlmeshv1alpha1.VirtualApp, pods []*v1.Pod) []*ctrlmeshproto.EndpointV1 {
	var endpoints []*ctrlmeshproto.EndpointV1
	for _, pod := range pods {
		if util.IsPodReady(pod) {
			endpoints = append(endpoints, &ctrlmeshproto.EndpointV1{
				Name:   pod.Name,
				Ip:     pod.Status.PodIP,
				Subset: determinePodSubset(vApp, pod),
			})
		}
	}
	if len(endpoints) > 0 {
		sort.SliceStable(endpoints, func(i, j int) bool {
			return endpoints[i].Name < endpoints[j].Name
		})
	}
	return endpoints
}

func isNamespaceMatched(ms ctrlmeshv1alpha1.MatchLimitSelector, ns *v1.Namespace) bool {
	if ms.NamespaceSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(ms.NamespaceSelector)
		if err != nil {
			klog.Warningf("Invalid namespace selector %v: %v", util.DumpJSON(ms.NamespaceSelector), err)
			return false
		}
		return selector.Matches(labels.Set(ns.Labels))
	} else if ms.NamespaceRegex != nil {
		regex, err := regexp.Compile(*ms.NamespaceRegex)
		if err != nil {
			klog.Warningf("Invalid namespace regex %v: %v", *ms.NamespaceRegex, err)
			return false
		}
		return regex.MatchString(ns.Name)
	}
	return false
}

func calculateSpecHash(spec *ctrlmeshproto.ProxySpecV1) string {
	return util.GetMD5Hash(util.DumpJSON(ctrlmeshproto.ProxySpecV1{
		Route:              spec.Route,
		Endpoints:          spec.Endpoints,
		ControlInstruction: spec.ControlInstruction,
	}))
}
