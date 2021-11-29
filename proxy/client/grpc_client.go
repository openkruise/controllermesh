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

	mu               sync.Mutex
	prevStatus       *ctrlmeshproto.ProxyStatusV1
	currentSnapshot  *ProtoSpecSnapshot
	historySnapshots []*ProtoSpecSnapshot
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
			connStream, err := grpcCtrlMeshClient.RegisterV1(ctx)
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

func (c *grpcClient) syncing(connStream ctrlmeshproto.ControllerMesh_RegisterV1Client, initChan chan struct{}) error {
	status := &ctrlmeshproto.ProxyStatusV1{SelfInfo: selfInfo}
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

func (c *grpcClient) send(connStream ctrlmeshproto.ControllerMesh_RegisterV1Client) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var leftSnapshots []*ProtoSpecSnapshot
	for _, s := range c.historySnapshots {
		if s.IsClosed() {
			continue
		}
		leftSnapshots = append(leftSnapshots, s)
	}
	c.historySnapshots = leftSnapshots

	if len(c.historySnapshots) > 0 {
		klog.Warningf("Skip report proto status for existing %v unclosed history snapshots yet", len(c.historySnapshots))
		return true, nil
	}

	newStatus := &ctrlmeshproto.ProxyStatusV1{}
	if c.currentSnapshot != nil {
		currentSpec, _, err := c.currentSnapshot.AcquireSpec()
		if err != nil {
			klog.Warningf("Skip report proto status for getting current spec from snapshot error: %v", err)
			return true, nil
		}
		defer c.currentSnapshot.ReleaseSpec()
		newStatus.CurrentSpec = currentSpec.ProxySpecV1
	}
	if util.IsJSONObjectEqual(newStatus, c.prevStatus) {
		return false, nil
	}

	if err := connStream.Send(newStatus); err != nil {
		return false, fmt.Errorf("send status %s error: %v", util.DumpJSON(newStatus), err)
	}
	c.prevStatus = newStatus
	return false, nil
}

func (c *grpcClient) recv(connStream ctrlmeshproto.ControllerMesh_RegisterV1Client) error {
	spec, err := connStream.Recv()
	if err != nil {
		return fmt.Errorf("receive spec error: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if spec == nil || spec.Meta == nil || spec.Route == nil {
		klog.Errorf("Receive invalid proto spec: %v", util.DumpJSON(spec))
		return nil
	}
	var renew bool
	if c.currentSnapshot != nil {
		renew = c.currentSnapshot.RenewOrClose(spec)
	}
	if !renew {
		if c.currentSnapshot != nil {
			c.historySnapshots = append(c.historySnapshots, c.currentSnapshot)
		}
		c.currentSnapshot = newProtoSpecSnapshot(spec)
	}

	msg := fmt.Sprintf("Receive proto spec, subset: '%v', hash: %v, controlInstruction: %v", spec.Route.Subset, spec.Meta.Hash, util.DumpJSON(spec.ControlInstruction))
	if klog.V(3).Enabled() {
		msg = fmt.Sprintf("%s, endpoints: %v", msg, util.DumpJSON(spec.Endpoints))
	}
	if klog.V(4).Enabled() {
		msg = fmt.Sprintf("%s, globalLimits: %v, subsetLimits: %v, subsetPublicResources: %v",
			msg, util.DumpJSON(spec.Route.GlobalLimits), util.DumpJSON(spec.Route.SubsetLimits), util.DumpJSON(spec.Route.SubsetPublicResources))
	}
	klog.Info(msg)
	return nil
}

func (c *grpcClient) GetProtoSpecSnapshot() *ProtoSpecSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentSnapshot
}
