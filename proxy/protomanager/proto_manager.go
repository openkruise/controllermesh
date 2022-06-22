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

package protomanager

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"

	"github.com/gogo/protobuf/proto"
	"k8s.io/klog/v2"

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	"github.com/openkruise/controllermesh/util"
)

var (
	selfInfo = &ctrlmeshproto.SelfInfo{Namespace: os.Getenv(constants.EnvPodNamespace), Name: os.Getenv(constants.EnvPodName)}
	onceInit sync.Once
)

type Client interface {
	Start(ctx context.Context) error
	GetSpecManager() *SpecManager
}

type SpecManager struct {
	sync.RWMutex
	storage           *storage
	reportTriggerChan chan struct{}

	expectedSpec *ctrlmeshproto.InternalSpec
	currentSpec  *ctrlmeshproto.InternalSpec
	unloadReason string

	requestsStore     *requestsStore
	requestsStoreLock sync.Mutex

	leaderElectionState *ctrlmeshproto.LeaderElectionStateV1
}

func newSpecManager(reportTriggerChan chan struct{}) (*SpecManager, error) {
	storage, err := newStorage()
	if err != nil {
		return nil, err
	}
	sm := &SpecManager{
		storage:           storage,
		reportTriggerChan: reportTriggerChan,
		requestsStore:     newRequestsStore(storage),
	}
	expectedSpec, currentSpec, err := sm.storage.loadData(sm.requestsStore)
	if currentSpec != nil {
		klog.Infof("Loaded currentSpec from storage: %v", util.DumpJSON(currentSpec))
		sm.currentSpec = ctrlmeshproto.ConvertProtoSpecToInternal(currentSpec)
	}
	if expectedSpec != nil {
		klog.Infof("Loaded expectedSpec from storage: %v", util.DumpJSON(expectedSpec))
		sm.UpdateSpec(expectedSpec)
	}
	return sm, nil
}

func (sm *SpecManager) UpdateLeaderElection(le *ctrlmeshproto.LeaderElectionStateV1) {
	sm.Lock()
	defer sm.Unlock()
	oldLe := sm.leaderElectionState
	sm.leaderElectionState = le
	if !proto.Equal(oldLe, le) {
		sm.reportTriggerChan <- struct{}{}
	}
}

func (sm *SpecManager) UpdateSpec(spec *ctrlmeshproto.ProxySpecV1) {
	sm.expectedSpec = ctrlmeshproto.ConvertProtoSpecToInternal(spec)
	if err := sm.storage.writeExpectedSpec(sm.expectedSpec.ProxySpecV1); err != nil {
		panic(fmt.Errorf("failed to write expected spec for %v: %v", util.DumpJSON(sm.expectedSpec.ProxySpecV1), err))
	}
	sm.Lock()
	defer sm.Unlock()
	if err := sm.checkLoadable(); err != nil {
		klog.Warningf("Check new spec is not loadable, because %v", err)
		sm.unloadReason = err.Error()
		return
	}
	sm.currentSpec = sm.expectedSpec
	if err := sm.storage.writeCurrentSpec(sm.currentSpec.ProxySpecV1); err != nil {
		panic(fmt.Errorf("failed to write current spec for %v: %v", util.DumpJSON(sm.currentSpec.ProxySpecV1), err))
	}
}

func (sm *SpecManager) GetStatus() *ctrlmeshproto.ProxyStatusV1 {
	sm.Lock()
	defer sm.Unlock()
	if sm.currentSpec == nil {
		return nil
	}
	return &ctrlmeshproto.ProxyStatusV1{
		MetaState: &ctrlmeshproto.MetaStateV1{
			Subset:           sm.currentSpec.Route.Subset,
			ExpectedHash:     sm.expectedSpec.Meta.Hash,
			CurrentHash:      sm.currentSpec.Meta.Hash,
			HashUnloadReason: sm.unloadReason,
		},
		LeaderElectionState: sm.leaderElectionState,
	}
}

func (sm *SpecManager) AcquireSpec() *ctrlmeshproto.InternalSpec {
	sm.RLock()
	return sm.currentSpec
}

func (sm *SpecManager) ReleaseSpec(req *ctrlmeshproto.ResourceRequest) {
	defer sm.RUnlock()
	if req == nil {
		return
	} else if req.ObjectSelector == nil && req.NamespacePassed.Len() == 0 && req.NamespaceDenied.Len() == 0 {
		return
	}
	sm.requestsStoreLock.Lock()
	defer sm.requestsStoreLock.Unlock()
	sm.requestsStore.add(req)
}

func (sm *SpecManager) checkLoadable() (err error) {
	if sm.currentSpec == nil {
		return nil
	}

	if sm.currentSpec.Meta.VAppName != sm.expectedSpec.Meta.VAppName {
		return fmt.Errorf("VAppName changed from %s to %s",
			sm.currentSpec.Meta.VAppName, sm.expectedSpec.Meta.VAppName)
	}
	if sm.currentSpec.Route.Subset != sm.expectedSpec.Route.Subset {
		return fmt.Errorf("subset changed from %s to %s",
			sm.currentSpec.Route.Subset, sm.expectedSpec.Route.Subset)
	}

	if sm.currentSpec.Meta.Hash == sm.expectedSpec.Meta.Hash {
		return nil
	}

	sm.requestsStoreLock.Lock()
	defer sm.requestsStoreLock.Unlock()
	sm.requestsStore.rangeRequests(func(req *ctrlmeshproto.ResourceRequest) bool {
		objectSelector := sm.expectedSpec.GetObjectSelector(req.GR)
		if !reflect.DeepEqual(objectSelector, req.ObjectSelector) {
			err = fmt.Errorf("object selector for %v changed from %v to %v",
				req.GR, util.DumpJSON(req.ObjectSelector), util.DumpJSON(objectSelector))
			return false
		}

		for _, ns := range req.NamespacePassed.UnsortedList() {
			if match := sm.expectedSpec.IsNamespaceMatch(ns, req.GR); !match {
				err = fmt.Errorf("namespace %s is already passed for %v, but it doesn't match the expected spec", ns, req.GR)
				return false
			}
		}
		for _, ns := range req.NamespaceDenied.UnsortedList() {
			if match := sm.expectedSpec.IsNamespaceMatch(ns, req.GR); match {
				err = fmt.Errorf("namespace %s is already denied for %v, but it matches the expected spec", ns, req.GR)
				return false
			}
		}
		return true
	})
	return
}
