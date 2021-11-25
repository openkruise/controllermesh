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
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
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
	//controllerKind       = ctrlmeshv1alpha1.SchemeGroupVersion.WithKind("VirtualApp")
)

type VirtualAppReconciler struct {
	client.Client
}

//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=virtualapps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=virtualapps/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=virtualapps/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

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

	protoRoute := generateProtoRoute(vApp, namespaces)
	protoEndpoints := generateProtoEndpoints(vApp, pods)

	// build diffStateForPods
	var diffStateForPods []podDiffState
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

		subset := determinePodSubset(vApp, pod)
		newSpec := &ctrlmeshproto.ProxySpecV1{
			Meta: &ctrlmeshproto.SpecMetaV1{VAppName: vApp.Name},
			Route: &ctrlmeshproto.RouteV1{
				Subset:                subset,
				GlobalLimits:          protoRoute.GlobalLimits,
				SubsetLimits:          protoRoute.SubsetLimits,
				SubsetPublicResources: protoRoute.SubsetPublicResources,
			},
			Endpoints:          protoEndpoints,
			ControlInstruction: &ctrlmeshproto.ControlInstructionV1{},
		}

		conn.mu.Lock()
		currentSpec := conn.status.currentSpec
		conn.mu.Unlock()

		diffStateForPods = append(diffStateForPods, podDiffState{
			pod:         pod,
			conn:        conn,
			newSpec:     newSpec,
			currentSpec: currentSpec,
		})
	}

	// check reload based on diffStateForPods
	var shouldReloadAll bool
	for _, diffState := range diffStateForPods {
		subset := diffState.newSpec.Route.Subset
		currentSpec := diffState.currentSpec
		pod := diffState.pod
		if currentSpec != nil && currentSpec.Route != nil {
			// If subset changed, reload all.
			if currentSpec.Route.Subset != subset {
				klog.Infof("VApp %s/%s find Pod %s subset changed from %s to %s, going to reload all.", vApp.Namespace, vApp.Name, pod.Name, currentSpec.Route.Subset, subset)
				shouldReloadAll = true
				continue
			}
			// If globalLimits changed, reload this pod.
			if !util.IsJSONObjectEqual(currentSpec.Route.GlobalLimits, protoRoute.GlobalLimits) {
				klog.Infof("VApp %s/%s find Pod %s globalLimits changed, going to reload it.", vApp.Namespace, vApp.Name, pod.Name)
				diffState.needReload = true
			}

			// If subsetPublicResources changed, reload it.
			if !util.IsJSONObjectEqual(currentSpec.Route.SubsetPublicResources, protoRoute.SubsetPublicResources) {
				klog.Infof("VApp %s/%s find Pod %s subsetPublicResources changed, going to reload it.", vApp.Namespace, vApp.Name, pod.Name)
				diffState.needReload = true
			}

			// If number of subset limits changed, reload all.
			newSubsetLimits := ctrlmeshproto.GetLimitsForSubset(subset, protoRoute.SubsetLimits)
			currentSubsetLimits := ctrlmeshproto.GetLimitsForSubset(subset, currentSpec.Route.SubsetLimits)
			if util.IsJSONObjectEqual(newSubsetLimits, currentSubsetLimits) {
				continue
			}

			// If number of subset limits changed, reload all.
			if len(newSubsetLimits) != len(currentSubsetLimits) {
				klog.Infof("VApp %s/%s find Pod %s number of subset limits changed from %s to %s, going to reload all.", vApp.Namespace, vApp.Name, pod.Name, len(currentSubsetLimits), len(newSubsetLimits))
				shouldReloadAll = true
				continue
			}

			// Get all namespaces added in subset limits
			namespacesAdded := sets.NewString()
			for i := 0; i < len(newSubsetLimits); i++ {
				newLimit := newSubsetLimits[i]
				currentLimit := currentSubsetLimits[i]
				if !util.IsJSONObjectEqual(newLimit.Resources, currentLimit.Resources) {
					klog.Infof("VApp %s/%s find Pod %s subset limit resourcesMatching changed, going to reload it.", vApp.Namespace, vApp.Name, pod.Name)
					diffState.needReload = true
				}
				// TODO: if objectSelector changed, we might have to reload all?
				namespacesAdded.Insert(sets.NewString(newLimit.Namespaces...).Delete(currentLimit.Namespaces...).UnsortedList()...)
			}
			// If there are some namespaced added, check if the namespaces already exist in current spec of other pods
			if namespacesAdded.Len() > 0 {
				for _, anotherDiffState := range diffStateForPods {
					anotherPod := anotherDiffState.pod
					if anotherPod.Name == pod.Name {
						continue
					}
					if anotherDiffState.currentSpec == nil || anotherDiffState.currentSpec.Route == nil {
						continue
					}
					currentLimitOfAnotherPod := ctrlmeshproto.GetLimitsForSubset(anotherDiffState.newSpec.Route.Subset, anotherDiffState.currentSpec.Route.SubsetLimits)
					for _, limit := range currentLimitOfAnotherPod {
						var isOverlapping bool
						for _, ns := range limit.Namespaces {
							if namespacesAdded.Has(ns) {
								klog.Infof("VApp %s/%s find Pod %s subset limit add namespace %s which already existed in pod %s, going to reload them.", vApp.Namespace, vApp.Name, pod.Name, ns, anotherPod.Name)
								diffState.needReload = true
								anotherDiffState.needReload = true
								isOverlapping = true
							}
						}
						if isOverlapping {
							break
						}
					}
				}
			}
		}
	}

	// sync messages based on diffStateForPods and shouldReloadAll
	for _, diffState := range diffStateForPods {

		newSpec := diffState.newSpec
		if shouldReloadAll || diffState.needReload {
			newSpec.ControlInstruction.BlockLeaderElection = true
		}
		newSpec.Meta.Hash = calculateSpecHash(newSpec)
		if diffState.currentSpec != nil && diffState.currentSpec.Meta != nil && diffState.currentSpec.Meta.Hash == newSpec.Meta.Hash {
			continue
		}

		klog.Infof("Preparing to send proto spec %v (hash: %v) to Pod %s/%s in VApp %s/%s", util.DumpJSON(newSpec), newSpec.Meta.Hash, diffState.pod.Namespace, diffState.pod.Name, vApp.Namespace, vApp.Name)
		expectationsSrvHash.Store(types.NamespacedName{Namespace: diffState.pod.Namespace, Name: diffState.pod.Name}, newSpec.Meta.Hash)
		if err := diffState.conn.srv.Send(newSpec); err != nil {
			expectationsSrvHash.Delete(types.NamespacedName{Namespace: diffState.pod.Namespace, Name: diffState.pod.Name})
			klog.Infof("Failed to send proto spec to Pod %s/%s in VApp %s/%s: %v", diffState.pod.Namespace, diffState.pod.Name, vApp.Namespace, vApp.Name, err)
			return reconcile.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

type podDiffState struct {
	pod         *v1.Pod
	conn        *grpcSrvConnection
	newSpec     *ctrlmeshproto.ProxySpecV1
	currentSpec *ctrlmeshproto.ProxySpecV1

	needReload bool
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
	sort.SliceStable(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
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
	sort.SliceStable(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })
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
