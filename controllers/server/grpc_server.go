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

package server

import (
	"context"
	"fmt"
	"io"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/grpcregistry"
	"github.com/openkruise/controllermesh/util"
)

var (
	grpcServer = &GrpcServer{}

	grpcRecvTriggerChannel = make(chan event.GenericEvent, 1024)

	// cachedGrpcSrvConnection type is map[types.NamespacedName]*grpcSrvConnection
	cachedGrpcSrvConnection = &sync.Map{}

	// expectationsSrvHash type is map[types.NamespacedName]string
	expectationsSrvHash = &sync.Map{}
)

type grpcSrvConnection struct {
	srv      ctrlmeshproto.ControllerMesh_RegisterV1Server
	stopChan chan struct{}
	status   grpcSrvStatus
	mu       sync.Mutex
}

type grpcSrvStatus struct {
	currentSpec *ctrlmeshproto.ProxySpecV1
}

func init() {
	_ = grpcregistry.Register("ctrlmesh-server", true, func(opts grpcregistry.RegisterOptions) {
		grpcServer.reader = opts.Mgr.GetCache()
		grpcServer.ctx = opts.Ctx
		ctrlmeshproto.RegisterControllerMeshServer(opts.GrpcServer, grpcServer)
	})
}

type GrpcServer struct {
	reader client.Reader
	ctx    context.Context
}

var _ ctrlmeshproto.ControllerMeshServer = &GrpcServer{}

func (s *GrpcServer) RegisterV1(srv ctrlmeshproto.ControllerMesh_RegisterV1Server) error {
	// receive the first register message
	pStatus, err := srv.Recv()
	if err != nil {
		return status.Errorf(codes.Aborted, err.Error())
	}
	if pStatus.SelfInfo == nil || pStatus.SelfInfo.Namespace == "" || pStatus.SelfInfo.Name == "" {
		return status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid selfInfo: %+v", pStatus.SelfInfo))
	}

	// get pod
	podNamespacedName := types.NamespacedName{Namespace: pStatus.SelfInfo.Namespace, Name: pStatus.SelfInfo.Name}
	pod := &v1.Pod{}
	if err := s.reader.Get(context.TODO(), podNamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			return status.Errorf(codes.NotFound, fmt.Sprintf("not found pod %s", podNamespacedName))
		}
		return status.Errorf(codes.Internal, fmt.Sprintf("get pod %s error: %v", podNamespacedName, err))
	} else if !util.IsPodActive(pod) {
		return status.Errorf(codes.Canceled, fmt.Sprintf("find pod %s inactive", podNamespacedName))
	}
	vAppName := pod.Labels[ctrlmeshv1alpha1.VirtualAppInjectedKey]
	if vAppName == "" {
		return status.Errorf(codes.InvalidArgument, fmt.Sprintf("empty %s label in pod %s", ctrlmeshv1alpha1.VirtualAppInjectedKey, podNamespacedName))
	}

	klog.V(3).Infof("Start proxy connection from Pod %s in VApp %s", podNamespacedName, vAppName)

	stopChan := make(chan struct{})
	conn := &grpcSrvConnection{srv: srv, stopChan: stopChan, status: grpcSrvStatus{currentSpec: pStatus.GetCurrentSpec()}}
	cachedGrpcSrvConnection.Store(podNamespacedName, conn)
	expectationsSrvHash.Delete(podNamespacedName)

	genericEvent := event.GenericEvent{Object: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: podNamespacedName.Namespace, Name: vAppName}}}
	grpcRecvTriggerChannel <- genericEvent
	go func() {
		for {
			pStatus, err = srv.Recv()
			if err != nil {
				if err == io.EOF {
					return
				}
				select {
				case <-srv.Context().Done():
				default:
					klog.Errorf("Receive error from Pod %s in VApp %s: %v", podNamespacedName, vAppName, err)
				}
				return
			}
			klog.Infof("Get proto status from Pod %s in VApp %s: %v", podNamespacedName, vAppName, util.DumpJSON(pStatus))

			if pStatus.GetCurrentSpec() != nil {
				var trigger bool
				conn.mu.Lock()
				if !util.IsJSONObjectEqual(conn.status.currentSpec, pStatus.GetCurrentSpec()) {
					trigger = true
				}
				// overwrite the whole status to avoid race condition
				conn.status = grpcSrvStatus{currentSpec: pStatus.GetCurrentSpec()}
				conn.mu.Unlock()
				if v, ok := expectationsSrvHash.Load(podNamespacedName); ok {
					expectHash := v.(string)
					var currentHash string
					if pStatus.GetCurrentSpec().GetMeta() != nil {
						currentHash = pStatus.GetCurrentSpec().GetMeta().GetHash()
					}
					if currentHash == expectHash {
						expectationsSrvHash.Delete(podNamespacedName)
					} else {
						klog.Warningf("Find unsatisfied hash %s (expected %s) in proto status from Pod %s in VApp %s", currentHash, expectHash, podNamespacedName, vAppName)
					}
				}
				if trigger {
					grpcRecvTriggerChannel <- genericEvent
				}
			}
		}
	}()

	select {
	case <-s.ctx.Done():
	case <-stopChan:
	case <-srv.Context().Done():
	}
	cachedGrpcSrvConnection.Delete(podNamespacedName)
	grpcRecvTriggerChannel <- genericEvent
	klog.V(3).Infof("Finish proxy connection from Pod %s in VApp %s", podNamespacedName, vAppName)
	return nil
}
