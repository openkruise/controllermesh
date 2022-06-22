/*
Copyright 2022 The Kruise Authors.

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

package fake

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fakev1beta1 "github.com/openkruise/controllermesh/test/fakecontroller/apis/fake/v1beta1"
)

var (
	podName = os.Getenv("POD_NAME")
)

const (
	ownerNameKey         = "owner-name"
	ownerTranslationsKey = "owner-transitions"
)

// FooReconciler reconciles a Foo object
type FooReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=fake.ctrlmesh.kruise.io,resources=foos,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fake.ctrlmesh.kruise.io,resources=foos/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=fake.ctrlmesh.kruise.io,resources=foos/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Foo object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *FooReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// TODO(user): your logic here
	klog.Infof("Reconciling object %v", req)

	var err error
	var translations int
	var skipUpdate bool
	foo := fakev1beta1.Foo{}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, req.NamespacedName, &foo); err != nil {
			return err
		}

		if foo.Labels[ownerNameKey] == podName {
			skipUpdate = true
			return nil
		}

		if foo.Labels == nil {
			foo.Labels = map[string]string{}
		}
		translations = 0
		if oldTrans := foo.Labels[ownerTranslationsKey]; len(oldTrans) > 0 {
			translations, err = strconv.Atoi(oldTrans)
			if err != nil {
				return fmt.Errorf("error convert owner-translations %v: %v", oldTrans, err)
			}
		}
		translations++
		foo.Labels[ownerNameKey] = podName
		foo.Labels[ownerTranslationsKey] = fmt.Sprintf("%d", translations)
		return r.Update(ctx, &foo)
	})

	if err != nil {
		klog.Warningf("Failed to mark Foo %v as owned: %v", req, err)
	} else if !skipUpdate {
		klog.Infof("Successfully mark Foo %v as owned, translations %v", req, translations)
	}
	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *FooReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fakev1beta1.Foo{}).
		Complete(r)
}
