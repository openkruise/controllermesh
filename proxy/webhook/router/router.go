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

package router

import (
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/openkruise/controllermesh/proxy/protomanager"
	"github.com/openkruise/controllermesh/util"
)

type Router interface {
	Route(req *admissionv1.AdmissionRequest) (*Accept, *Redirect, *Ignore, *Error)
}

type Accept struct{}
type Redirect struct {
	Hosts []string
}
type Ignore struct{}

type Error struct {
	Code int
	Msg  string
}

func New(m *protomanager.SpecManager) Router {
	return &router{specManager: m}
}

type router struct {
	specManager *protomanager.SpecManager
}

func (r *router) Route(req *admissionv1.AdmissionRequest) (*Accept, *Redirect, *Ignore, *Error) {
	gr := schema.GroupResource{Group: req.Resource.Group, Resource: req.Resource.Resource}

	protoSpec := r.specManager.AcquireSpec()
	defer r.specManager.ReleaseSpec(nil)

	if protoSpec.IsDefaultAndEmpty() {
		return &Accept{}, nil, nil, nil
	}

	ignore, self, hosts := protoSpec.GetMatchedSubsetEndpoint(req.Namespace, gr)
	if ignore {
		return nil, nil, &Ignore{}, nil
	}
	if self {
		return &Accept{}, nil, nil, nil
	}
	if len(hosts) == 0 {
		return nil, nil, nil, &Error{Code: http.StatusNotFound, Msg: fmt.Sprintf("find no endpoints for request %v", util.DumpJSON(req))}
	}
	return nil, &Redirect{Hosts: hosts}, nil, nil
}
