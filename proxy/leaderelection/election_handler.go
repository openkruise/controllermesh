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

	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog/v2"

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	"github.com/openkruise/controllermesh/proxy/protomanager"
	"github.com/openkruise/controllermesh/util"
)

type Handler interface {
	Handle(*request.RequestInfo, *http.Request) (bool, func(response *http.Response) error, error)
}

func New(specManger *protomanager.SpecManager, lockName string) Handler {
	protoSpec := specManger.AcquireSpec()
	defer specManger.ReleaseSpec(nil)
	return &handler{
		specManger: specManger,
		namespace:  os.Getenv(constants.EnvPodNamespace),
		lockName:   lockName,
		subset:     protoSpec.Route.Subset,
	}
}

type handler struct {
	specManger *protomanager.SpecManager
	namespace  string
	lockName   string
	subset     string
}

func (h *handler) Handle(req *request.RequestInfo, r *http.Request) (handled bool, modifyResponse func(response *http.Response) error, retErr error) {
	if !req.IsResourceRequest || req.Subresource != "" {
		return false, nil, nil
	}
	if req.Namespace != h.namespace {
		return false, nil, nil
	} else if req.Verb != "create" && req.Name != h.lockName && !strings.HasPrefix(req.Name, h.lockName+"---") {
		return false, nil, nil
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
		return false, nil, nil
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
			return true, nil, err
		}

		if adp.GetName() != h.lockName {
			return false, nil, nil
		}

		holdIdentity, ok := adp.GetHoldIdentity()
		if !ok {
			return true, nil, fmt.Errorf("find no hold identity resource lock")
		}

		modifyResponse = func(resp *http.Response) error {
			if resp.StatusCode == http.StatusOK {
				h.specManger.UpdateLeaderElection(&ctrlmeshproto.LeaderElectionStateV1{
					Identity: holdIdentity,
					IsLeader: true,
				})
			}
			return nil
		}

		if h.subset != "" {
			name := setSubsetIntoName(h.lockName, h.subset)
			adp.SetName(name)
		}

		adp.EncodeInto(r)

		return true, modifyResponse, nil

	case "update":
		if err := adp.DecodeFrom(r); err != nil {
			return true, nil, err
		}

		holdIdentity, ok := adp.GetHoldIdentity()
		if !ok {
			return true, nil, fmt.Errorf("find no hold identity resource lock")
		}

		modifyResponse = func(resp *http.Response) error {
			if resp.StatusCode == http.StatusOK {
				h.specManger.UpdateLeaderElection(&ctrlmeshproto.LeaderElectionStateV1{
					Identity: holdIdentity,
					IsLeader: true,
				})
			}
			return nil
		}

		// Do NOT modify the lock name again for update request

		adp.EncodeInto(r)

		return true, modifyResponse, nil

	case "get":
		if h.subset != "" {
			r.URL.Path = util.LastReplace(r.URL.Path, h.lockName, setSubsetIntoName(h.lockName, h.subset))
		}
		return true, nil, nil

	default:
		klog.Infof("Ignore %s lock operation", req.Verb)
	}
	return false, nil, nil
}

func setSubsetIntoName(name, subset string) string {
	return fmt.Sprintf("%s---%s", name, subset)
}
