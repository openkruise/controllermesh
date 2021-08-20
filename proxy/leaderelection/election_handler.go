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

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"

	proxyclient "github.com/openkruise/controllermesh/proxy/client"
	clientset "k8s.io/client-go/kubernetes"

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
)

var runtimeScheme = runtime.NewScheme()
var runtimeSerializer runtime.Serializer

func init() {
	utilruntime.Must(v1.AddToScheme(runtimeScheme))
	utilruntime.Must(coordinationv1.AddToScheme(runtimeScheme))
	mediaTypes := serializer.NewCodecFactory(runtimeScheme).SupportedMediaTypes()
	for _, info := range mediaTypes {
		if info.MediaType == "application/json" {
			runtimeSerializer = info.Serializer
		}
	}
}

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

func (h *handler) Handle(req *request.RequestInfo, r *http.Request) (bool, *Error) {
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
	klog.V(5).Infof("Handling %s resource lock %s %s", req.Verb, req.Resource, req.Name)

	var isStrictHashChanged bool
	select {
	case <-h.routeSnapshot.Ctx.Done():
		klog.Infof("Preparing to reject leader election for route snapshot with strict hash %s canceled.", h.routeSnapshot.SpecHash.RouteStrictHash)
		isStrictHashChanged = true
	default:
		h.routeSnapshot.Mutex.Lock()
		defer h.routeSnapshot.Mutex.Unlock()
	}

	switch req.Verb {

	case "create":
		if err := adp.DecodeFrom(r.Body); err != nil {
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

		if isStrictHashChanged {
			if h.identity != holdIdentity && h.lastTranslationTime.After(h.routeSnapshot.RefreshTime) {
				close(h.routeSnapshot.Closed)
				h.routeSnapshot = h.proxyClient.GetProtoSpecSnapshot()
				klog.Infof("Starting route strict hash %s with a new Leader Election ID %s", h.routeSnapshot.SpecHash.RouteStrictHash, holdIdentity)
				// still reject, for next trusted get
			}
			// it means this identity may have ListWatch of old hash
			return true, &Error{Code: http.StatusExpectationFailed, Err: fmt.Errorf("strict hash changed")}
		}

		if h.routeSnapshot.Route.Subset != "" {
			name := setSubsetIntoName(h.lockName, h.routeSnapshot.Route.Subset)
			adp.SetName(name)
			r.URL.Path = strings.Replace(r.URL.Path, h.lockName, name, -1)
		}

		adp.EncodeInto(r)

		return true, nil

	case "update":
		return true, nil

	case "get":
		if isStrictHashChanged {
			return true, &Error{Code: http.StatusNotFound, Err: fmt.Errorf("fake not found for strict hash changed")}
		}
		if h.routeSnapshot.ControlInstruction != nil && h.routeSnapshot.ControlInstruction.BlockLeaderElection {
			return true, &Error{Code: http.StatusNotAcceptable, Err: fmt.Errorf("blocking leader election")}
		}
		if h.routeSnapshot.Route.Subset != "" {
			r.URL.Path = strings.Replace(r.URL.Path, h.lockName, setSubsetIntoName(h.lockName, h.routeSnapshot.Route.Subset), -1)
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
