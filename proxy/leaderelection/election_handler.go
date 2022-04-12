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

package leaderelection

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/request"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	proxyclient "github.com/openkruise/controllermesh/proxy/client"
	"github.com/openkruise/controllermesh/util"
)

type Handler interface {
	Handle(*request.RequestInfo, *http.Request) (bool, *Error)
}

type Error struct {
	Code int
	Err  error
}

func New(cli clientset.Interface, proxyClient proxyclient.Client, lockName string) Handler {
	snapshot := proxyClient.GetProtoSpecSnapshot()
	return &handler{client: cli, proxyClient: proxyClient, namespace: os.Getenv(constants.EnvPodNamespace), lockName: lockName, routeSnapshot: snapshot}
}

type handler struct {
	client      clientset.Interface
	proxyClient proxyclient.Client
	namespace   string
	lockName    string

	routeSnapshot       *proxyclient.ProtoSpecSnapshot
	identity            string
	lastTranslationTime time.Time
}

func (h *handler) Handle(req *request.RequestInfo, r *http.Request) (handled bool, retErr *Error) {
	if !req.IsResourceRequest || req.Subresource != "" {
		return false, nil
	}
	if req.Namespace != h.namespace {
		return false, nil
	} else if req.Verb != "create" && req.Name != h.lockName && !strings.HasPrefix(req.Name, h.lockName+"---") {
		return false, nil
	}

	var adp adapter
	gvr := schema.GroupVersionResource{Group: req.APIGroup, Version: req.APIVersion, Resource: req.Resource}
	switch gvr {
	case v1.SchemeGroupVersion.WithResource("configmaps"):
		adp = newObjectAdapter(&v1.ConfigMap{})
	case v1.SchemeGroupVersion.WithResource("endpoints"):
		adp = newObjectAdapter(&v1.Endpoints{})
	case coordinationv1.SchemeGroupVersion.WithResource("leases"):
		adp = newLeaseAdapter()
	default:
		return false, nil
	}
	defer func() {
		if retErr != nil {
			klog.Warningf("Error handling %s resource lock %s %s, %+v", req.Verb, req.Resource, req.Name, retErr)
		} else {
			klog.V(6).Infof("Successfully handling %s resource lock %s %s", req.Verb, req.Resource, req.Name)
		}
	}()

	switch req.Verb {

	case "create":
		if err := adp.DecodeFrom(r); err != nil {
			return true, &Error{Code: http.StatusBadRequest, Err: err}
		}

		if adp.GetName() != h.lockName {
			return false, nil
		}

		holdIdentity, ok := adp.GetHoldIdentity()
		if !ok {
			return true, &Error{Code: http.StatusBadRequest, Err: fmt.Errorf("find no hold identity resource lock")}
		}

		defer func() {
			h.identity = holdIdentity
			h.lastTranslationTime = time.Now()
		}()

		protoSpec, refreshTime, err := h.routeSnapshot.AcquireSpec()
		if err != nil {
			if h.identity != holdIdentity && h.lastTranslationTime.After(refreshTime) {
				h.routeSnapshot = h.proxyClient.GetProtoSpecSnapshot()
				klog.Infof("Starting new proto spec with new Leader Election ID %s", holdIdentity)
				// still reject, for next trusted get
			} else {
				klog.V(5).Infof("Find snapshot exceeded, waiting for re-elect, last identity: %v, current identity: %v, last translation time: %v, snapshot refreshTime: %v",
					h.identity, holdIdentity, h.lastTranslationTime, refreshTime)
			}
			// it means this identity may have ListWatch of old hash
			return true, &Error{Code: http.StatusExpectationFailed, Err: fmt.Errorf("snapshot exceeded")}
		}
		defer h.routeSnapshot.ReleaseSpec()

		if protoSpec.Route.Subset != "" {
			name := setSubsetIntoName(h.lockName, protoSpec.Route.Subset)
			adp.SetName(name)
		}

		adp.EncodeInto(r)

		return true, nil

	case "update":
		return true, nil

	case "get":
		protoSpec, _, err := h.routeSnapshot.AcquireSpec()
		if err != nil {
			klog.Warningf("Rejecting election get for route snapshot exceeded")
			return true, &Error{Code: http.StatusNotFound, Err: fmt.Errorf("fake not found for route snapshot exceeded")}
		}
		defer h.routeSnapshot.ReleaseSpec()

		if protoSpec.ControlInstruction != nil && protoSpec.ControlInstruction.BlockLeaderElection {
			return true, &Error{Code: http.StatusNotAcceptable, Err: fmt.Errorf("blocking leader election")}
		}
		if protoSpec.Route.Subset != "" {
			r.URL.Path = util.LastReplace(r.URL.Path, h.lockName, setSubsetIntoName(h.lockName, protoSpec.Route.Subset))
		}
		return true, nil

	default:
		klog.Infof("Ignore %s lock operation", req.Verb)
	}
	return false, nil
}

func setSubsetIntoName(name, subset string) string {
	return fmt.Sprintf("%s---%s", name, subset)
}
