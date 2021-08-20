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

package clustermeta

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/grpcregistry"
	"github.com/openkruise/controllermesh/util"
	webhookutil "github.com/openkruise/controllermesh/webhook/util"
)

var (
	podSelector labels.Selector
	namespace   string
	localName   string
)

// ClusterMetaReconciler reconciles a ClusterMeta object
type ClusterMetaReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=clustermeta,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=clustermeta/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=clustermeta/finalizers,verbs=update

func (r *ClusterMetaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	if req.Name != ctrlmeshv1alpha1.NameOfManager {
		klog.Infof("Ignore ClusterMeta %s", req.Name)
		return reconcile.Result{}, nil
	}

	start := time.Now()
	klog.V(3).Infof("Starting to process ClusterMeta %v", req.Name)
	defer func() {
		if err != nil {
			klog.Warningf("Failed to process ClusterMeta %v, elapsedTime %v, error: %v", req.Name, time.Since(start), err)
		} else {
			klog.Infof("Finish to process ClusterMeta %v, elapsedTime %v", req.Name, time.Since(start))
		}
	}()

	podList := &v1.PodList{}
	err = r.List(context.TODO(), podList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: podSelector})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("list pods in %s error: %v", namespace, err)
	}

	var hasLeader bool
	endpoints := make(ctrlmeshv1alpha1.ClusterMetaEndpoints, 0, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		if !util.IsPodActive(pod) {
			continue
		}

		e := ctrlmeshv1alpha1.ClusterMetaEndpoint{Name: pod.Name, PodIP: pod.Status.PodIP}
		if pod.Name == localName {
			e.Leader = true
			hasLeader = true
		}
		endpoints = append(endpoints, e)
	}
	sort.Sort(endpoints)
	if !hasLeader {
		return reconcile.Result{}, fmt.Errorf("no leader %s in new endpoints %v", localName, util.DumpJSON(endpoints))
	}

	ports := ctrlmeshv1alpha1.ClusterMetaPorts{}
	ports.GrpcLeaderElectionPort, ports.GrpcNonLeaderElectionPort = grpcregistry.GetGrpcPorts()
	newStatus := ctrlmeshv1alpha1.ClusterMetaStatus{
		Namespace: namespace,
		Endpoints: endpoints,
		Ports:     &ports,
	}

	metaInfo := &ctrlmeshv1alpha1.ClusterMeta{}
	err = r.Get(context.TODO(), req.NamespacedName, metaInfo)
	if err != nil {
		if !errors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("get ClusterMeta %s error: %v", req.Name, err)
		}

		metaInfo.Name = ctrlmeshv1alpha1.NameOfManager
		metaInfo.Status = newStatus
		err = r.Create(context.TODO(), metaInfo)
		if err != nil && !errors.IsAlreadyExists(err) {
			return reconcile.Result{}, fmt.Errorf("create ClusterMeta %s error: %v", req.Name, err)
		}
		return
	}

	if reflect.DeepEqual(metaInfo.Status, newStatus) {
		return reconcile.Result{}, nil
	}

	metaInfo.Status = newStatus
	err = r.Status().Update(context.TODO(), metaInfo)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("update ClusterMeta %s error: %v", req.Name, err)
	}

	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterMetaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	namespace = webhookutil.GetNamespace()
	if localName = os.Getenv("POD_NAME"); len(localName) == 0 {
		return fmt.Errorf("find no POD_NAME in env")
	}

	// Read the service of ctrlmesh-manager's webhook, to get the pod selector
	svc := &v1.Service{}
	svcNamespacedName := types.NamespacedName{Namespace: namespace, Name: webhookutil.GetServiceName()}
	err := mgr.GetAPIReader().Get(context.TODO(), svcNamespacedName, svc)
	if err != nil {
		return fmt.Errorf("get service %s error: %v", svcNamespacedName, err)
	}

	podSelector, err = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: svc.Spec.Selector})
	if err != nil {
		return fmt.Errorf("parse service %s selector %v error: %v", svcNamespacedName, svc.Spec.Selector, err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&ctrlmeshv1alpha1.ClusterMeta{}).
		Watches(&source.Kind{Type: &v1.Pod{}}, &enqueueHandler{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				pod := e.Object.(*v1.Pod)
				return podSelector.Matches(labels.Set(pod.Labels))
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				pod := e.ObjectNew.(*v1.Pod)
				return podSelector.Matches(labels.Set(pod.Labels))
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				pod := e.Object.(*v1.Pod)
				return podSelector.Matches(labels.Set(pod.Labels))
			},
			GenericFunc: func(e event.GenericEvent) bool {
				pod := e.Object.(*v1.Pod)
				return podSelector.Matches(labels.Set(pod.Labels))
			},
		})).
		Complete(r)
}

type enqueueHandler struct{}

func (e *enqueueHandler) Create(_ event.CreateEvent, q workqueue.RateLimitingInterface) {
	q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
		Name: ctrlmeshv1alpha1.NameOfManager,
	}})
}

func (e *enqueueHandler) Update(_ event.UpdateEvent, q workqueue.RateLimitingInterface) {
	q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
		Name: ctrlmeshv1alpha1.NameOfManager,
	}})
}

func (e *enqueueHandler) Delete(_ event.DeleteEvent, q workqueue.RateLimitingInterface) {
	q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
		Name: ctrlmeshv1alpha1.NameOfManager,
	}})
}

func (e *enqueueHandler) Generic(_ event.GenericEvent, q workqueue.RateLimitingInterface) {
	q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
		Name: ctrlmeshv1alpha1.NameOfManager,
	}})
}
