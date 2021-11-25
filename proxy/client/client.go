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

package client

import (
	"context"
	"os"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
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
	GetProtoSpecSnapshot() *ProtoSpecSnapshot
}

type ProtoSpecSnapshot struct {
	ctx    context.Context
	cancel context.CancelFunc

	mtx      sync.Mutex
	refCount int32
	specLock sync.RWMutex

	spec        *ctrlmeshproto.InternalSpec
	refreshTime time.Time

	allowedNamespaces sets.String
	deniedNamespaces  sets.String
}

func newProtoSpecSnapshot(spec *ctrlmeshproto.ProxySpecV1) *ProtoSpecSnapshot {
	ctx, cancel := context.WithCancel(context.Background())
	return &ProtoSpecSnapshot{
		ctx:               ctx,
		cancel:            cancel,
		spec:              ctrlmeshproto.ConvertProtoSpecToInternal(spec),
		refreshTime:       time.Now(),
		allowedNamespaces: sets.NewString(),
		deniedNamespaces:  sets.NewString(),
	}
}

func (ss *ProtoSpecSnapshot) isExceeded() bool {
	select {
	case <-ss.ctx.Done():
		return true
	default:
	}
	return false
}

func (ss *ProtoSpecSnapshot) IsClosed() bool {
	if !ss.isExceeded() {
		return false
	}
	ss.mtx.Lock()
	defer ss.mtx.Unlock()
	return ss.refCount <= 0
}

func (ss *ProtoSpecSnapshot) AcquireSpec() (*ctrlmeshproto.InternalSpec, time.Time, error) {
	ss.mtx.Lock()
	defer ss.mtx.Unlock()
	ss.specLock.RLock()
	if ss.isExceeded() {
		ss.specLock.RUnlock()
		return ss.spec, ss.refreshTime, context.Canceled
	}
	ss.refCount++
	return ss.spec, ss.refreshTime, nil
}

func (ss *ProtoSpecSnapshot) RecordNamespace(allowed, denied []string) {
	ss.mtx.Lock()
	defer ss.mtx.Unlock()
	if len(allowed) > 0 {
		ss.allowedNamespaces.Insert(allowed...)
	}
	if len(denied) > 0 {
		ss.deniedNamespaces.Insert(denied...)
	}
}

func (ss *ProtoSpecSnapshot) ReleaseSpec() {
	ss.mtx.Lock()
	defer ss.mtx.Unlock()
	ss.refCount--
	ss.specLock.RUnlock()
}

func (ss *ProtoSpecSnapshot) RenewOrClose(newSpec *ctrlmeshproto.ProxySpecV1) (renew bool) {
	defer func() {
		// close this snapshot if it can not renew
		if !renew {
			klog.Warningf("Cancel current snapshot for it can not renew proto spec.")
			ss.cancel()
		}
	}()

	if util.IsJSONObjectEqual(newSpec, ss.spec) {
		return true
	}

	ss.specLock.Lock()
	defer ss.specLock.Unlock()

	// close if blockLeaderElection turns to true
	if newSpec.ControlInstruction.BlockLeaderElection && !ss.spec.ControlInstruction.BlockLeaderElection {
		klog.Warningf("Can not renew proto snapshot for blockLeaderElection changed")
		return false
	}

	// compare route in spec
	if newSpec.Route.Subset != ss.spec.Route.Subset {
		klog.Warningf("Can not renew proto snapshot for subset changed")
		return false
	} else if !util.IsJSONObjectEqual(newSpec.Route.GlobalLimits, ss.spec.Route.GlobalLimits) {
		klog.Warningf("Can not renew proto snapshot for globalLimits changed")
		return false
	} else if !util.IsJSONObjectEqual(newSpec.Route.SubsetPublicResources, ss.spec.Route.SubsetPublicResources) {
		klog.Warningf("Can not renew proto snapshot for subsetPublicResources changed, old: %v, new: %v",
			util.DumpJSON(ss.spec.Route.SubsetPublicResources), util.DumpJSON(newSpec.Route.SubsetPublicResources))
		return false
	}

	newSubsetLimits := ctrlmeshproto.GetLimitsForSubset(newSpec.Route.Subset, newSpec.Route.SubsetLimits)
	currentSubsetLimits := ctrlmeshproto.GetLimitsForSubset(newSpec.Route.Subset, ss.spec.Route.SubsetLimits)
	if !util.IsJSONObjectEqual(newSubsetLimits, currentSubsetLimits) {
		if len(newSubsetLimits) != len(currentSubsetLimits) {
			klog.Warningf("Can not renew proto snapshot for length of subset limits changed")
			return false
		}
		namespacesAdded := sets.NewString()
		namespacesRemoved := sets.NewString()
		for i := 0; i < len(newSubsetLimits); i++ {
			newLimit := newSubsetLimits[i]
			currentLimit := currentSubsetLimits[i]
			if util.IsJSONObjectEqual(newLimit, currentLimit) {
				continue
			}
			if !util.IsJSONObjectEqual(newLimit.Resources, currentLimit.Resources) {
				klog.Warningf("Can not renew proto snapshot for resources in subset limits changed")
				return false
			}
			namespacesAdded.Insert(sets.NewString(newLimit.Namespaces...).Delete(currentLimit.Namespaces...).UnsortedList()...)
			namespacesRemoved.Insert(sets.NewString(currentLimit.Namespaces...).Delete(newLimit.Namespaces...).UnsortedList()...)
		}
		removeAllowedNamespaces := ss.allowedNamespaces.Intersection(namespacesRemoved)
		addDeniedNamespaces := ss.deniedNamespaces.Intersection(namespacesAdded)
		if removeAllowedNamespaces.Len() > 0 || addDeniedNamespaces.Len() > 0 {
			klog.Warningf("Can not renew proto snapshot for it has allowed new remove-namespaces %v or has denied new allow-namespaces %v", removeAllowedNamespaces.List(), addDeniedNamespaces.List())
			return false
		}
	}

	ss.spec = ctrlmeshproto.ConvertProtoSpecToInternal(newSpec)
	ss.refreshTime = time.Now()
	return true
}
