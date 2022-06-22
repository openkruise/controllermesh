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
	"math/rand"
	"time"

	"github.com/gogo/protobuf/proto"
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

	reportTriggerChan chan struct{}
	specManager       *SpecManager
}

func NewGrpcClient() Client {
	return &grpcClient{reportTriggerChan: make(chan struct{}, 1000)}
}

func (c *grpcClient) Start(ctx context.Context) (err error) {
	c.specManager, err = newSpecManager(c.reportTriggerChan)
	if err != nil {
		return fmt.Errorf("error new spec manager: %v", err)
	}

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
	// Do the first send for self info
	firstStatus := c.specManager.GetStatus()
	if firstStatus == nil {
		firstStatus = &ctrlmeshproto.ProxyStatusV1{}
	}
	firstStatus.SelfInfo = selfInfo
	klog.Infof("Preparing to send first status: %v", util.DumpJSON(firstStatus))
	if err := connStream.Send(firstStatus); err != nil {
		return fmt.Errorf("send first status %s error: %v", util.DumpJSON(firstStatus), err)
	}

	stopChan := make(chan struct{})
	defer close(stopChan)
	go func() {
		// wait for the first recv
		<-initChan

		var prevStatus *ctrlmeshproto.ProxyStatusV1
		sendTimer := time.NewTimer(time.Minute)
		for {
			select {
			case <-c.reportTriggerChan:
			case <-sendTimer.C:
			case <-stopChan:
				return
			}
			sendTimer.Stop()
			if status, err := c.send(connStream, prevStatus); err != nil {
				klog.Errorf("Failed to send message: %v", err)
			} else {
				// 30 ~ 60s
				sendTimer.Reset(time.Second*30 + time.Millisecond*time.Duration(rand.Intn(6000)))
				if status != nil {
					prevStatus = status
				}
			}
		}
	}()

	isFirstTime := true
	for {
		if isFirstTime {
			klog.V(4).Infof("Waiting for the first time recv...")
		}
		err := c.recv(connStream)
		if err != nil {
			return fmt.Errorf("recv error: %v", err)
		}
		isFirstTime = false
		onceInit.Do(func() {
			close(initChan)
		})
		c.reportTriggerChan <- struct{}{}
	}
}

func (c *grpcClient) send(connStream ctrlmeshproto.ControllerMesh_RegisterV1Client, prevStatus *ctrlmeshproto.ProxyStatusV1) (*ctrlmeshproto.ProxyStatusV1, error) {
	newStatus := c.specManager.GetStatus()
	if newStatus == nil {
		klog.Infof("Skip to send gRPC status for it is nil.")
		return nil, nil
	}
	if proto.Equal(newStatus, prevStatus) {
		return nil, nil
	}

	klog.Infof("Preparing to send new status: %v", util.DumpJSON(newStatus))
	if err := connStream.Send(newStatus); err != nil {
		return nil, fmt.Errorf("send status error: %v", err)
	}
	return newStatus, nil
}

func (c *grpcClient) recv(connStream ctrlmeshproto.ControllerMesh_RegisterV1Client) error {
	spec, err := connStream.Recv()
	if err != nil {
		return fmt.Errorf("receive spec error: %v", err)
	}

	if spec == nil || spec.Meta == nil || spec.Route == nil {
		klog.Errorf("Receive invalid proto spec: %v", util.DumpJSON(spec))
		return nil
	}

	msg := fmt.Sprintf("Receive proto spec, subset: '%v', hash: %v", spec.Route.Subset, spec.Meta.Hash)
	if klog.V(3).Enabled() {
		msg = fmt.Sprintf("%s, endpoints: %v", msg, util.DumpJSON(spec.Endpoints))
	}
	if klog.V(4).Enabled() {
		msg = fmt.Sprintf("%s, globalLimits: %v, subsetLimits: %v, subsetPublicResources: %v",
			msg, util.DumpJSON(spec.Route.GlobalLimits), util.DumpJSON(spec.Route.SubsetLimits), util.DumpJSON(spec.Route.SubsetPublicResources))
	}
	klog.Info(msg)

	c.specManager.UpdateSpec(spec)
	return nil
}

func (c *grpcClient) GetSpecManager() *SpecManager {
	return c.specManager
}
