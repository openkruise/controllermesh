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
	"flag"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/util"
)

var (
	concurrentReconciles = flag.Int("ctrlmesh-server-workers", 3, "Max concurrent workers for CtrlMesh Server controller.")
	//controllerKind       = ctrlmeshv1alpha1.SchemeGroupVersion.WithKind("VirtualOperator")

	randToken = rand.String(10)
)

type VirtualAppReconciler struct {
	client.Client
}

//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=virtualapps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=virtualapps/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=virtualapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

func (r *VirtualAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, retErr error) {
	startTime := time.Now()
	defer func() {
		if retErr == nil {
			if res.Requeue || res.RequeueAfter > 0 {
				klog.Infof("Finished syncing VApp %s, cost %v, result: %v", req, time.Since(startTime), res)
			} else {
				klog.Infof("Finished syncing VApp %s, cost %v", req, time.Since(startTime))
			}
		} else {
			klog.Errorf("Failed syncing VApp %s: %v", req, retErr)
		}
	}()

	vApp := &ctrlmeshv1alpha1.VirtualApp{}
	err := r.Get(context.TODO(), req.NamespacedName, vApp)
	if err != nil {
		if errors.IsNotFound(err) {
			// TODO: need to reset all Pods?
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	pods, err := r.getPodsForVApp(vApp)
	if err != nil {
		return reconcile.Result{}, err
	}

	// wait for all pods satisfied
	for _, pod := range pods {
		if v, ok := expectationsSrvHash.Load(types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}); ok {
			klog.Warningf("Skip reconcile VApp %s for Pod %s has dirty expectation %s", req, pod.Name, util.DumpJSON(v))
			return reconcile.Result{}, nil
		}
	}

	namespaces, err := r.getActiveNamespaces()
	if err != nil {
		return reconcile.Result{}, err
	}

	protoRouteMap := generateProtoRoute(vApp, namespaces)
	protoEndpoints := generateProtoEndpoints(vApp, pods)

	var existsOldStrictHash bool
	var podSpecsList []podSpecs
	for _, pod := range pods {
		var conn *grpcSrvConnection
		if v, ok := cachedGrpcSrvConnection.Load(types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}); !ok || v == nil {
			if pod.Status.Phase == v1.PodRunning && util.IsPodReady(pod) {
				klog.Warningf("VApp %s/%s find no connection from ready Pod %s yet", vApp.Namespace, vApp.Name, pod.Name)
				return reconcile.Result{RequeueAfter: time.Minute}, nil
			}
			continue
		} else {
			conn = v.(*grpcSrvConnection)
		}

		newSpec := &ctrlmeshproto.ProxySpec{
			Meta: &ctrlmeshproto.VAppMeta{
				Token:           randToken,
				Name:            vApp.Name,
				ResourceVersion: vApp.ResourceVersion,
			},
			Route:              protoRouteMap[determinePodSubset(vApp, pod)],
			Endpoints:          protoEndpoints,
			ControlInstruction: &ctrlmeshproto.ControlInstruction{},
		}
		newSpecHash := util.CalculateHashForProtoSpec(newSpec)

		conn.mu.Lock()
		prevStatus := conn.status
		conn.mu.Unlock()

		var isSpecHashConsistent, isStrictHashConsistent bool
		if prevStatus.specHash != nil {
			if prevStatus.specHash.RouteStrictHash != newSpecHash.RouteStrictHash {
				existsOldStrictHash = true
			} else {
				isStrictHashConsistent = true
			}
			if prevStatus.specHash.RouteHash == newSpecHash.RouteHash &&
				prevStatus.specHash.EndpointsHash == newSpecHash.EndpointsHash &&
				prevStatus.specHash.NamespacesHash == newSpecHash.NamespacesHash {
				isSpecHashConsistent = true
			}
		}

		var isBlockingLeaderElection bool
		if prevStatus.controlInstruction != nil && prevStatus.controlInstruction.BlockLeaderElection {
			isBlockingLeaderElection = true
		}

		podSpecsList = append(podSpecsList, podSpecs{
			pod:         pod,
			conn:        conn,
			newSpec:     newSpec,
			newSpecHash: newSpecHash,

			isSpecHashConsistent:     isSpecHashConsistent,
			isStrictHashConsistent:   isStrictHashConsistent,
			isBlockingLeaderElection: isBlockingLeaderElection,
		})
	}

	for _, podSpecs := range podSpecsList {
		shouldOpenLeaderElection := podSpecs.isBlockingLeaderElection && !existsOldStrictHash
		if !shouldOpenLeaderElection && podSpecs.isSpecHashConsistent {
			continue
		}

		if existsOldStrictHash && !podSpecs.isStrictHashConsistent {
			podSpecs.newSpec.ControlInstruction.BlockLeaderElection = existsOldStrictHash
		}
		klog.Infof("Sending proto spec %v (hash: %v) to Pod %s in VApp %s/%s", util.DumpJSON(podSpecs.newSpec), util.DumpJSON(podSpecs.newSpecHash), podSpecs.pod.Name, vApp.Namespace, vApp.Name)
		expectationsSrvHash.Store(types.NamespacedName{Namespace: podSpecs.pod.Namespace, Name: podSpecs.pod.Name}, podSpecs.newSpecHash)
		if err := podSpecs.conn.srv.Send(podSpecs.newSpec); err != nil {
			expectationsSrvHash.Delete(types.NamespacedName{Namespace: podSpecs.pod.Namespace, Name: podSpecs.pod.Name})
			klog.Infof("Failed to send proto spec to Pod %s in VApp %s/%s: %v", podSpecs.pod.Name, vApp.Namespace, vApp.Name, err)
			return reconcile.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

type podSpecs struct {
	pod         *v1.Pod
	conn        *grpcSrvConnection
	newSpec     *ctrlmeshproto.ProxySpec
	newSpecHash *ctrlmeshproto.SpecHash

	isSpecHashConsistent     bool
	isStrictHashConsistent   bool
	isBlockingLeaderElection bool
}

func (r *VirtualAppReconciler) getPodsForVApp(vApp *ctrlmeshv1alpha1.VirtualApp) ([]*v1.Pod, error) {
	podList := v1.PodList{}
	if err := r.List(context.TODO(), &podList, client.InNamespace(vApp.Namespace), client.MatchingLabels{ctrlmeshv1alpha1.VirtualAppInjectedKey: vApp.Name}); err != nil {
		return nil, err
	}
	var pods []*v1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if util.IsPodActive(pod) {
			pods = append(pods, pod)
		}
	}
	return pods, nil
}

func (r *VirtualAppReconciler) getActiveNamespaces() ([]*v1.Namespace, error) {
	namespaceList := v1.NamespaceList{}
	if err := r.List(context.TODO(), &namespaceList); err != nil {
		return nil, err
	}
	var namespaces []*v1.Namespace
	for i := range namespaceList.Items {
		ns := &namespaceList.Items[i]
		if ns.DeletionTimestamp == nil {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: *concurrentReconciles}).
		For(&ctrlmeshv1alpha1.VirtualApp{}).
		Watches(&source.Kind{Type: &v1.Pod{}}, &podEventHandler{reader: mgr.GetCache()}).
		Watches(&source.Kind{Type: &v1.Namespace{}}, &namespaceEventHandler{reader: mgr.GetCache()}).
		Watches(&source.Channel{Source: grpcRecvTriggerChannel}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
