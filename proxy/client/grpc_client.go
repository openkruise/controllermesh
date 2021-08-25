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
	"fmt"
	"reflect"
	"sync"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/client"
	ctrlmeshv1alphainformers "github.com/openkruise/controllermesh/client/informers/externalversions/ctrlmesh/v1alpha1"
	ctrlmeshv1alpha1listers "github.com/openkruise/controllermesh/client/listers/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/util"
)

type grpcClient struct {
	informer cache.SharedIndexInformer
	lister   ctrlmeshv1alpha1listers.ManagerStateLister

	meta               *ctrlmeshproto.VAppMeta
	route              *ctrlmeshproto.InternalRoute
	endpoints          []*ctrlmeshproto.Endpoint
	specHash           *ctrlmeshproto.SpecHash
	controlInstruction *ctrlmeshproto.ControlInstruction
	refreshTime        time.Time
	mu                 sync.Mutex

	snapshots  []*ProtoSpecSnapshot
	prevStatus *ctrlmeshproto.ProxyStatus
}

func NewGrpcClient() Client {
	return &grpcClient{}
}

func (c *grpcClient) Start(ctx context.Context) error {
	clientset := client.GetGenericClient().CtrlmeshClient
	c.informer = ctrlmeshv1alphainformers.NewFilteredManagerStateInformer(clientset, 0, cache.Indexers{}, func(opts *metav1.ListOptions) {
		opts.FieldSelector = "metadata.name=" + ctrlmeshv1alpha1.NameOfManager
	})
	c.lister = ctrlmeshv1alpha1listers.NewManagerStateLister(c.informer.GetIndexer())

	go func() {
		c.informer.Run(ctx.Done())
	}()
	if ok := cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced); !ok {
		return fmt.Errorf("error waiting ManagerState informer synced")
	}

	initChan := make(chan struct{})
	go c.connect(ctx, initChan)
	<-initChan
	return nil
}

func (c *grpcClient) connect(ctx context.Context, initChan chan struct{}) {
	for i := 0; ; i++ {
		if i > 0 {
			time.Sleep(time.Second * 3)
		}
		klog.V(4).Infof("Starting grpc connecting...")

		managerState, err := c.lister.Get(ctrlmeshv1alpha1.NameOfManager)
		if err != nil {
			if errors.IsNotFound(err) {
				klog.Warningf("Not found ManagerState %s, waiting...", ctrlmeshv1alpha1.NameOfManager)
			} else {
				klog.Warningf("Failed to get ManagerState %s: %v, waiting...", ctrlmeshv1alpha1.NameOfManager, err)
			}
			continue
		}

		if managerState.Status.Ports == nil || managerState.Status.Ports.GrpcLeaderElectionPort == 0 {
			klog.Warningf("No grpc port in ManagerState %s, waiting...", util.DumpJSON(managerState))
			continue
		}

		var leader *ctrlmeshv1alpha1.ManagerStateEndpoint
		for i := range managerState.Status.Endpoints {
			e := &managerState.Status.Endpoints[i]
			if e.Leader {
				leader = e
				break
			}
		}
		if leader == nil {
			klog.Warningf("No leader in ManagerState %s, waiting...", util.DumpJSON(managerState))
			continue
		}

		addr := fmt.Sprintf("%s:%d", leader.PodIP, managerState.Status.Ports.GrpcLeaderElectionPort)
		klog.V(4).Infof("Preparing to connect ctrlmesh-manager %v", addr)
		func() {
			var opts []grpc.DialOption
			opts = append(opts, grpc.WithInsecure())
			grpcConn, err := grpc.Dial(addr, opts...)
			if err != nil {
				klog.Errorf("Failed to grpc connect to ctrlmesh-manager %s addr %s: %v", leader.Name, addr, err)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			defer func() {
				cancel()
				_ = grpcConn.Close()
			}()

			grpcCtrlMeshClient := ctrlmeshproto.NewControllerMeshClient(grpcConn)
			connStream, err := grpcCtrlMeshClient.Register(ctx)
			if err != nil {
				klog.Errorf("Failed to register to ctrlmesh-manager %s addr %s: %v", leader.Name, addr, err)
				return
			}

			if err := c.syncing(connStream, initChan); err != nil {
				klog.Errorf("Failed syncing grpc connection to ctrlmesh-manager %s addr %s: %v", leader.Name, addr, err)
			}
		}()
	}
}

func (c *grpcClient) syncing(connStream ctrlmeshproto.ControllerMesh_RegisterClient, initChan chan struct{}) error {
	status := &ctrlmeshproto.ProxyStatus{SelfInfo: selfInfo}
	if err := connStream.Send(status); err != nil {
		return fmt.Errorf("send first status %s error: %v", util.DumpJSON(status), err)
	}

	stopChan := make(chan struct{})
	defer close(stopChan)
	triggerChan := make(chan struct{}, 1000)
	go func() {
		sendTimer := time.NewTimer(time.Second)
		for {
			select {
			case <-triggerChan:
			case <-sendTimer.C:
			case <-stopChan:
				return
			}
			sendTimer.Stop()
			if retry, err := c.send(connStream); err != nil {
				klog.Errorf("Failed to send message: %v", err)
			} else if retry {
				sendTimer.Reset(time.Second)
			} else {
				sendTimer.Reset(time.Minute)
			}
		}
	}()

	for {
		err := c.recv(connStream)
		if err != nil {
			return fmt.Errorf("recv error: %v", err)
		}
		onceInit.Do(func() { close(initChan) })
		triggerChan <- struct{}{}
	}
}

func (c *grpcClient) send(connStream ctrlmeshproto.ControllerMesh_RegisterClient) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var notConsistentCount int
	var newSnapshots []*ProtoSpecSnapshot
	for _, s := range c.snapshots {
		select {
		case <-s.Closed:
			continue
		default:
		}
		if s.SpecHash.RouteStrictHash != c.specHash.RouteStrictHash {
			s.Cancel()
			notConsistentCount++
		} else if s.SpecHash != c.specHash || reflect.DeepEqual(s.ControlInstruction, c.controlInstruction) {
			s.Mutex.Lock()
			s.SpecHash = c.specHash
			s.Route = c.route
			s.Endpoints = c.endpoints
			s.ControlInstruction = c.controlInstruction
			s.Mutex.Unlock()
		}
		newSnapshots = append(newSnapshots, s)
	}
	c.snapshots = newSnapshots

	if notConsistentCount > 0 {
		klog.Warningf("Skip report proto status for strict hash has %v not been consistent yet", notConsistentCount)
		return true, nil
	}

	newStatus := &ctrlmeshproto.ProxyStatus{SpecHash: c.specHash, ControlInstruction: c.controlInstruction}
	if reflect.DeepEqual(newStatus, c.prevStatus) {
		return false, nil
	}

	if err := connStream.Send(newStatus); err != nil {
		return false, fmt.Errorf("send status %s error: %v", util.DumpJSON(newStatus), err)
	}
	return false, nil
}

func (c *grpcClient) recv(connStream ctrlmeshproto.ControllerMesh_RegisterClient) error {
	spec, err := connStream.Recv()
	if err != nil {
		return fmt.Errorf("receive spec error: %v", err)
	}

	route := &ctrlmeshproto.InternalRoute{}
	if err = route.DecodeFrom(spec.Route); err != nil {
		klog.Errorf("Failed to decode from spec route %v: %v", util.DumpJSON(spec.Route), err)
		return nil
	}

	oldSpecHash := c.specHash
	if c.meta != nil && spec.Meta != nil && c.meta.Name != spec.Meta.Name {
		return fmt.Errorf("get VAppMeta name in spec changed %s -> %s", c.meta.Name, spec.Meta.Name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.meta = spec.Meta
	c.route = route
	c.endpoints = spec.Endpoints
	c.specHash = util.CalculateHashForProtoSpec(spec)
	c.controlInstruction = spec.ControlInstruction
	c.refreshTime = time.Now()
	klog.V(1).Infof("Refresh new proto spec, subset: '%v', globalLimits: %s, subRules: %s, subsets: %s, sensSubsetNamespaces: %+v. endpoints: %s. control: %s. SpecHash from %+v to %+v",
		route.Subset, util.DumpJSON(route.GlobalLimits), util.DumpJSON(route.SubRules), util.DumpJSON(route.Subsets), route.SensSubsetNamespaces,
		util.DumpJSON(spec.Endpoints), util.DumpJSON(spec.ControlInstruction), oldSpecHash, c.specHash)
	return nil
}

func (c *grpcClient) GetProtoSpec() (*ctrlmeshproto.VAppMeta, *ctrlmeshproto.InternalRoute, []*ctrlmeshproto.Endpoint, *ctrlmeshproto.ControlInstruction, *ctrlmeshproto.SpecHash, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.meta, c.route, c.endpoints, c.controlInstruction, c.specHash, c.refreshTime
}

func (c *grpcClient) GetProtoSpecSnapshot() *ProtoSpecSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	s := &ProtoSpecSnapshot{
		Ctx:                ctx,
		Cancel:             cancel,
		Closed:             make(chan struct{}),
		Route:              c.route,
		Endpoints:          c.endpoints,
		SpecHash:           c.specHash,
		ControlInstruction: c.controlInstruction,
		RefreshTime:        c.refreshTime,
	}
	c.snapshots = append(c.snapshots, s)
	return s
}
