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

	"github.com/gogo/protobuf/proto"
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

// TODO: proxy container should exit after controller container
// if we need a new gRPC method to trigger proxy exit when controller exited?

var (
	grpcServer = &GrpcServer{}

	grpcRecvTriggerChannel = make(chan event.GenericEvent, 1024)

	// cachedGrpcSrvConnection type is map[types.UID]*grpcSrvConnection
	cachedGrpcSrvConnection = &sync.Map{}
)

func init() {
	_ = grpcregistry.Register("ctrlmesh-server", true, func(opts grpcregistry.RegisterOptions) {
		grpcServer.reader = opts.Mgr.GetCache()
		grpcServer.ctx = opts.Ctx
		ctrlmeshproto.RegisterControllerMeshServer(opts.GrpcServer, grpcServer)
	})
}

type grpcSrvConnection struct {
	mu     sync.Mutex
	srv    ctrlmeshproto.ControllerMesh_RegisterV1Server
	status *ctrlmeshproto.ProxyStatusV1

	sendTimes    int
	disconnected bool
}

func (conn *grpcSrvConnection) send(spec *ctrlmeshproto.ProxySpecV1) error {
	conn.mu.Lock()
	conn.sendTimes++
	conn.mu.Unlock()
	return conn.srv.Send(spec)
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

	if pStatus.MetaState == nil {
		klog.Infof("Start first-time connection from Pod %s in VApp %s", podNamespacedName, vAppName)
	} else {
		klog.Infof("Start re-connection from Pod %s in VApp %s", podNamespacedName, vAppName)
	}

	conn := &grpcSrvConnection{srv: srv, status: pStatus}
	cachedGrpcSrvConnection.Store(pod.UID, conn)
	podHashExpectation.Delete(pod.UID)

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

			conn.mu.Lock()
			statusChanged := !proto.Equal(conn.status, pStatus)
			// overwrite the whole status to avoid race condition
			conn.status = pStatus
			conn.mu.Unlock()

			if statusChanged {
				grpcRecvTriggerChannel <- genericEvent
			}
		}
	}()

	select {
	case <-s.ctx.Done():
		return nil
	case <-srv.Context().Done():
	}
	klog.Infof("Stop connection from Pod %s in VApp %s", podNamespacedName, vAppName)
	podHashExpectation.Delete(pod.UID)
	// Can NOT delete this conn in cachedGrpcSrvConnection
	conn.mu.Lock()
	conn.disconnected = true
	conn.mu.Unlock()
	grpcRecvTriggerChannel <- genericEvent
	return nil
}
