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

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	"k8s.io/apimachinery/pkg/runtime/schema"

	proxyclient "github.com/openkruise/controllermesh/proxy/client"
	admissionv1 "k8s.io/api/admission/v1"
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

func New(c proxyclient.Client) Router {
	return &router{proxyClient: c}
}

type router struct {
	proxyClient proxyclient.Client
}

func (r *router) Route(req *admissionv1.AdmissionRequest) (*Accept, *Redirect, *Ignore, *Error) {
	snapshot := r.proxyClient.GetProtoSpecSnapshot()
	protoSpec, _, err := snapshot.AcquireSpec()
	if err != nil {
		return nil, nil, nil, &Error{Code: http.StatusExpectationFailed, Msg: "route snapshot exceeded changed"}
	}
	defer snapshot.ReleaseSpec()

	gr := schema.GroupResource{Group: req.Resource.Group, Resource: req.Resource.Resource}
	if protoSpec.RouteInternal.IsDefaultAndEmpty() {
		return &Accept{}, nil, nil, nil
	}
	matchSubset, ok := protoSpec.RouteInternal.DetermineNamespaceSubset(req.Namespace, gr)
	if !ok {
		return nil, nil, &Ignore{}, nil
	}

	if matchSubset == ctrlmeshproto.AllSubsetPublic || matchSubset == protoSpec.RouteInternal.Subset {
		return &Accept{}, nil, nil, nil
	}

	var hosts []string
	for _, e := range protoSpec.Endpoints {
		if e.Subset == matchSubset {
			hosts = append(hosts, e.Ip)
		}
	}

	if len(hosts) == 0 {
		return nil, nil, nil, &Error{Code: http.StatusNotFound, Msg: fmt.Sprintf("find no endpoints for subset %s", matchSubset)}
	}

	return nil, &Redirect{Hosts: hosts}, nil, nil
}
