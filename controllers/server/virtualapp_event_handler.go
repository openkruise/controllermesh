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
	"reflect"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/util"
)

type podEventHandler struct {
	reader client.Reader
}

func (h *podEventHandler) Create(e event.CreateEvent, q workqueue.RateLimitingInterface) {
	pod := e.Object.(*v1.Pod)
	vApp, err := h.getVAppForPod(pod)
	if err != nil {
		klog.Warningf("Failed to get VApp for Pod %s/%s creation: %v", pod.Namespace, pod.Name, err)
		return
	} else if vApp == nil {
		return
	}
	q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: vApp.Namespace,
		Name:      vApp.Name,
	}})
}

func (h *podEventHandler) Update(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
	oldPod := e.ObjectOld.(*v1.Pod)
	newPod := e.ObjectNew.(*v1.Pod)

	if newPod.DeletionTimestamp != nil {
		return
	}

	vApp, err := h.getVAppForPod(newPod)
	if err != nil {
		klog.Warningf("Failed to get VApp for Pod %s/%s update: %v", newPod.Namespace, newPod.Name, err)
		return
	} else if vApp == nil {
		return
	}

	// if the subset of pod changed, enqueue it
	oldSubset := determinePodSubset(vApp, oldPod)
	newSubset := determinePodSubset(vApp, newPod)
	if oldSubset != newSubset || util.IsPodReady(oldPod) != util.IsPodReady(newPod) {
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: vApp.Namespace,
			Name:      vApp.Name,
		}})
	}
}

func (h *podEventHandler) Delete(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
}

func (h *podEventHandler) Generic(e event.GenericEvent, q workqueue.RateLimitingInterface) {
}

func (h *podEventHandler) getVAppForPod(pod *v1.Pod) (*ctrlmeshv1alpha1.VirtualApp, error) {
	name := pod.Labels[ctrlmeshv1alpha1.VirtualAppInjectedKey]
	if name == "" {
		return nil, nil
	}
	vApp := &ctrlmeshv1alpha1.VirtualApp{}
	err := h.reader.Get(context.TODO(), types.NamespacedName{Namespace: pod.Namespace, Name: name}, vApp)
	return vApp, err
}

type namespaceEventHandler struct {
	reader client.Reader
}

func (h *namespaceEventHandler) Create(e event.CreateEvent, q workqueue.RateLimitingInterface) {
	vApps := h.getSensitiveVApps()
	for _, vApp := range vApps {
		h.enqueue(vApp, q)
	}
}

func (h *namespaceEventHandler) Update(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
	newNS := e.ObjectNew.(*v1.Namespace)
	oldNS := e.ObjectOld.(*v1.Namespace)
	if reflect.DeepEqual(newNS.Labels, oldNS.Labels) {
		return
	}

	vApps := h.getSensitiveVApps()
	for _, vApp := range vApps {
		var diff bool
		for _, ms := range vApp.Spec.Route.GlobalLimits {
			oldMatch, _ := ms.IsNamespaceMatched(oldNS)
			newMatch, _ := ms.IsNamespaceMatched(newNS)
			if oldMatch != newMatch {
				diff = true
				break
			}
		}
		if diff {
			h.enqueue(vApp, q)
			continue
		}

		for _, r := range vApp.Spec.Route.SubRules {
			for _, ms := range r.Match {
				oldMatch, _ := ms.IsNamespaceMatched(oldNS)
				newMatch, _ := ms.IsNamespaceMatched(newNS)
				if oldMatch != newMatch {
					diff = true
					break
				}
			}
			if diff {
				break
			}
		}
		if diff {
			h.enqueue(vApp, q)
		}
	}
}

func (h *namespaceEventHandler) Delete(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
	vApps := h.getSensitiveVApps()
	for _, vApp := range vApps {
		h.enqueue(vApp, q)
	}
}

func (h *namespaceEventHandler) Generic(e event.GenericEvent, q workqueue.RateLimitingInterface) {
}

func (h *namespaceEventHandler) getSensitiveVApps() []*ctrlmeshv1alpha1.VirtualApp {
	vAppList := ctrlmeshv1alpha1.VirtualAppList{}
	if err := h.reader.List(context.TODO(), &vAppList); err != nil {
		klog.Errorf("Failed to list all vApps: %v", err)
		return nil
	}
	var vApps []*ctrlmeshv1alpha1.VirtualApp
	for i := range vAppList.Items {
		vApp := &vAppList.Items[i]
		if len(vApp.Spec.Route.GlobalLimits) == 0 && len(vApp.Spec.Route.SubRules) == 0 {
			continue
		}
		vApps = append(vApps, vApp)
	}
	return vApps
}

func (h *namespaceEventHandler) enqueue(vApp *ctrlmeshv1alpha1.VirtualApp, q workqueue.RateLimitingInterface) {
	q.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: vApp.Namespace, Name: vApp.Name}}, time.Millisecond*200)
}
