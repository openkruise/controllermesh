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

package virtualapp

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"regexp"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
)

type ValidatingHandler struct {
	// Decoder decodes objects
	Decoder *admission.Decoder
}

var _ admission.Handler = &ValidatingHandler{}

// Handle handles admission requests.
func (h *ValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &ctrlmeshv1alpha1.VirtualApp{}
	err := h.Decoder.Decode(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if err := validate(obj); err != nil {
		return admission.Errored(http.StatusUnprocessableEntity, err)
	}

	if req.AdmissionRequest.Operation == admissionv1.Update {
		oldObj := &ctrlmeshv1alpha1.VirtualApp{}
		if err := h.Decoder.DecodeRaw(req.AdmissionRequest.OldObject, oldObj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if err := validateUpdate(obj, oldObj); err != nil {
			return admission.Errored(http.StatusUnprocessableEntity, err)
		}
	}

	return admission.ValidationResponse(true, "")
}

func validate(obj *ctrlmeshv1alpha1.VirtualApp) error {
	if selector, err := metav1.LabelSelectorAsSelector(obj.Spec.Selector); err != nil {
		return fmt.Errorf("invalid selector: %v", err)
	} else if selector.Empty() || selector.String() == "" {
		return fmt.Errorf("invalid selector can not be empty")
	}

	if len(obj.Spec.Subsets) > 0 {
		if obj.Spec.Configuration.Controller == nil && obj.Spec.Configuration.Webhook == nil {
			return fmt.Errorf("must set controller or webhook in configuration, for subsets defined")
		}
	}

	if obj.Spec.Configuration.Controller != nil {
		if obj.Spec.Configuration.Controller.LeaderElectionName == "" {
			return fmt.Errorf("leaderElectionName for controller can not be empty")
		}
	}
	if obj.Spec.Configuration.Webhook != nil {
		if obj.Spec.Configuration.Webhook.CertDir == "" {
			return fmt.Errorf("certDir for webhook can not be empty")
		}
		if obj.Spec.Configuration.Webhook.Port <= 0 {
			return fmt.Errorf("port for webhook must be bigger than 0")
		}
	}

	subRules := sets.NewString()
	if obj.Spec.Route != nil {
		for _, m := range obj.Spec.Route.GlobalLimits {
			if err := validateMatchLimitSelector(m, false); err != nil {
				return fmt.Errorf("%s in globalLimits", err)
			}
		}
		for i := range obj.Spec.Route.SubRules {
			r := &obj.Spec.Route.SubRules[i]
			if r.Name == "" {
				return fmt.Errorf("empty subRule name")
			}
			if subRules.Has(r.Name) {
				return fmt.Errorf("duplicated %s in subRules", r.Name)
			}
			if len(r.Match) == 0 {
				return fmt.Errorf("no match defined in subRule %s", r.Name)
			}
			for _, m := range r.Match {
				if err := validateMatchLimitSelector(m, true); err != nil {
					return fmt.Errorf("%s in subRule %s", err, r.Name)
				}
			}
			subRules.Insert(r.Name)
		}
	}

	subsets := sets.NewString()
	usedRules := sets.NewString()
	for i := range obj.Spec.Subsets {
		s := &obj.Spec.Subsets[i]
		if s.Name == "" {
			return fmt.Errorf("empty subset name")
		}
		if subsets.Has(s.Name) {
			return fmt.Errorf("duplicated %s in subsets", s.Name)
		}
		subsets.Insert(s.Name)

		if len(s.Labels) == 0 {
			return fmt.Errorf("no labels defined in subset %s", s.Name)
		}
		if len(s.RouteRules) == 0 {
			return fmt.Errorf("no routeRules defined in subset %s", s.Name)
		}
		for _, name := range s.RouteRules {
			if !subRules.Has(name) {
				return fmt.Errorf("not found rule %s in subset %s", name, s.Name)
			}
			if usedRules.Has(name) {
				return fmt.Errorf("rule %s in subset %s has been used by front subset", name, s.Name)
			}
			usedRules.Insert(name)
		}
	}

	return nil
}

func validateMatchLimitSelector(m ctrlmeshv1alpha1.MatchLimitSelector, shouldNotHaveObjectSelector bool) error {
	switch {
	case m.NamespaceSelector != nil:
		if _, err := metav1.LabelSelectorAsSelector(m.NamespaceSelector); err != nil {
			return fmt.Errorf("parse namespaceSelector error: %v", err)
		}
	case m.NamespaceRegex != nil:
		if _, err := regexp.Compile(*m.NamespaceRegex); err != nil {
			return fmt.Errorf("parse namespaceRegex error: %v", err)
		}
	case m.ObjectSelector != nil:
		if shouldNotHaveObjectSelector {
			return fmt.Errorf("object selector can not be defined in subRules")
		}
		if _, err := metav1.LabelSelectorAsSelector(m.ObjectSelector); err != nil {
			return fmt.Errorf("parse ObjectSelector error: %v", err)
		}
	default:
		return fmt.Errorf("empty match limit selector")
	}
	if (m.NamespaceSelector != nil && m.NamespaceRegex != nil) ||
		(m.ObjectSelector != nil && m.NamespaceRegex != nil) ||
		(m.NamespaceSelector != nil && m.ObjectSelector != nil) {
		return fmt.Errorf("invalid match limit selector")
	}
	if m.ObjectSelector != nil && m.Resources == nil {
		return fmt.Errorf("invalid object selector, no resource specified")
	}
	return nil
}

func validateUpdate(obj, oldObj *ctrlmeshv1alpha1.VirtualApp) error {
	if !reflect.DeepEqual(obj.Spec.Selector, oldObj.Spec.Selector) {
		return fmt.Errorf("selector can not be modified")
	}
	return nil
}

var _ admission.DecoderInjector = &ValidatingHandler{}

func (h *ValidatingHandler) InjectDecoder(d *admission.Decoder) error {
	h.Decoder = d
	return nil
}
