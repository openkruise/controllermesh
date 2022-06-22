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
	r := rs.requestsByGR[req.GR.String()]
	if r == nil {
		rs.requestsByGR[req.GR.String()] = req
	} else {
		if !reflect.DeepEqual(r.ObjectSelector, req.ObjectSelector) {
			klog.Warningf("Find new resource request %s has different object selector with the old %v",
				util.DumpJSON(req), util.DumpJSON(req.ObjectSelector))
			r.ObjectSelector = req.ObjectSelector
		}
		r.NamespacePassed = r.NamespacePassed.Union(req.NamespacePassed)
		r.NamespaceDenied = r.NamespaceDenied.Union(req.NamespaceDenied)
	}

	if err := rs.storage.writeHistoryRequests(rs); err != nil {
		panic(fmt.Errorf("failed to write history requests for %v: %v", util.DumpJSON(req), err))
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
