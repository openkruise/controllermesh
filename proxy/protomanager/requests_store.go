/*
Copyright 2022 The Kruise Authors.

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

package protomanager

import (
	"encoding/json"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	"github.com/openkruise/controllermesh/util"
)

type requestsStore struct {
	storage *storage
	pack    requestsPack
}

type requestsPack struct {
	RequestsByGR    map[string]*ctrlmeshproto.ResourceRequest `json:"requestsByGR,omitempty"`
	UserAgentPassed sets.String                               `json:"userAgentPassed,omitempty"`
	UserAgentDenied sets.String                               `json:"userAgentDenied,omitempty"`
}

func newRequestsStore(s *storage) *requestsStore {
	return &requestsStore{
		storage: s,
		pack: requestsPack{
			RequestsByGR:    map[string]*ctrlmeshproto.ResourceRequest{},
			UserAgentPassed: sets.NewString(),
			UserAgentDenied: sets.NewString(),
		},
	}
}

func (rs *requestsStore) add(req *ctrlmeshproto.ResourceRequest) {
	if req == nil {
		return
	}

	var changed bool

	if req.UserAgentPassed != nil {
		if !rs.pack.UserAgentPassed.Has(*req.UserAgentPassed) {
			rs.pack.UserAgentPassed.Insert(*req.UserAgentPassed)
			changed = true
		}
	} else if req.UserAgentDenied != nil {
		if !rs.pack.UserAgentDenied.Has(*req.UserAgentDenied) {
			klog.Infof("Additional user-agent denied: %v", *req.UserAgentDenied)
			rs.pack.UserAgentDenied.Insert(*req.UserAgentDenied)
			changed = true
		}
	}

	if req.ObjectSelector != nil || req.NamespacePassed.Len() > 0 || req.NamespaceDenied.Len() > 0 {
		reqByGR := rs.pack.RequestsByGR[req.GR.String()]
		if reqByGR == nil {
			klog.Infof("GroupVersion %s initial record: %v", req.GR.String(), util.DumpJSON(req))
			rs.pack.RequestsByGR[req.GR.String()] = req
			changed = true
		} else {
			if !reflect.DeepEqual(reqByGR.ObjectSelector, req.ObjectSelector) {
				klog.Warningf("Find %s has new object selector %s different with the old %v",
					req.GR.String(), util.DumpJSON(req.ObjectSelector), util.DumpJSON(reqByGR.ObjectSelector))
				reqByGR.ObjectSelector = req.ObjectSelector
				changed = true
			}
			if diff := req.NamespacePassed.Difference(reqByGR.NamespacePassed); diff.Len() > 0 {
				klog.Infof("GroupVersion %s has additional namespaces passed: %v", req.GR.String(), diff.List())
				reqByGR.NamespacePassed.Insert(diff.UnsortedList()...)
				changed = true
			}
			if diff := req.NamespaceDenied.Difference(reqByGR.NamespaceDenied); diff.Len() > 0 {
				klog.Infof("GroupVersion %s has additional namespaces denied: %v", req.GR.String(), diff.List())
				reqByGR.NamespaceDenied.Insert(diff.UnsortedList()...)
				changed = true
			}
		}
	}

	if changed {
		if err := rs.storage.writeHistoryRequests(rs); err != nil {
			panic(fmt.Errorf("failed to write history requests for %v: %v", util.DumpJSON(req), err))
		}
	}
}

func (rs *requestsStore) rangeRequests(fn func(*ctrlmeshproto.ResourceRequest) bool) {
	for _, userAgent := range rs.pack.UserAgentPassed.UnsortedList() {
		if ok := fn(&ctrlmeshproto.ResourceRequest{UserAgentPassed: &userAgent}); !ok {
			break
		}
	}
	for _, userAgent := range rs.pack.UserAgentDenied.UnsortedList() {
		if ok := fn(&ctrlmeshproto.ResourceRequest{UserAgentDenied: &userAgent}); !ok {
			break
		}
	}
	for _, req := range rs.pack.RequestsByGR {
		if ok := fn(req); !ok {
			break
		}
	}
}

func (rs *requestsStore) marshal() ([]byte, error) {
	return json.Marshal(rs.pack)
}

func (rs *requestsStore) unmarshal(data []byte) error {
	return json.Unmarshal(data, &rs.pack)
}
