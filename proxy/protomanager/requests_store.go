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

	"k8s.io/klog/v2"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	"github.com/openkruise/controllermesh/util"
)

type requestsStore struct {
	storage      *storage
	requestsByGR map[string]*ctrlmeshproto.ResourceRequest
}

func newRequestsStore(s *storage) *requestsStore {
	return &requestsStore{
		storage:      s,
		requestsByGR: map[string]*ctrlmeshproto.ResourceRequest{},
	}
}

func (rs *requestsStore) add(req *ctrlmeshproto.ResourceRequest) {
	var changed bool
	r := rs.requestsByGR[req.GR.String()]
	if r == nil {
		klog.Infof("GroupVersion %s initial record: %v", req.GR.String(), util.DumpJSON(req))
		rs.requestsByGR[req.GR.String()] = req
		changed = true
	} else {
		if !reflect.DeepEqual(r.ObjectSelector, req.ObjectSelector) {
			klog.Warningf("Find %s has new object selector %s different with the old %v",
				req.GR.String(), util.DumpJSON(req.ObjectSelector), util.DumpJSON(r.ObjectSelector))
			r.ObjectSelector = req.ObjectSelector
			changed = true
		}
		if diff := req.NamespacePassed.Difference(r.NamespacePassed); diff.Len() > 0 {
			klog.Infof("GroupVersion %s has additional namespaces passed: %v", req.GR.String(), diff.List())
			r.NamespacePassed.Insert(diff.UnsortedList()...)
			changed = true
		}
		if diff := req.NamespaceDenied.Difference(r.NamespaceDenied); diff.Len() > 0 {
			klog.Infof("GroupVersion %s has additional namespaces denied: %v", req.GR.String(), diff.List())
			r.NamespaceDenied.Insert(diff.UnsortedList()...)
			changed = true
		}
	}

	if changed {
		if err := rs.storage.writeHistoryRequests(rs); err != nil {
			panic(fmt.Errorf("failed to write history requests for %v: %v", util.DumpJSON(req), err))
		}
	}
}

func (rs *requestsStore) rangeRequests(fn func(*ctrlmeshproto.ResourceRequest) bool) {
	for _, req := range rs.requestsByGR {
		if ok := fn(req); !ok {
			break
		}
	}
}

func (rs *requestsStore) marshal() ([]byte, error) {
	return json.Marshal(rs.requestsByGR)
}

func (rs *requestsStore) unmarshal(data []byte) error {
	return json.Unmarshal(data, &rs.requestsByGR)
}
