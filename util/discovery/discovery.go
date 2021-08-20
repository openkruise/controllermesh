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

package discovery

import (
	"fmt"
	"sync"

	"github.com/openkruise/controllermesh/client"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

var (
	errKindNotFound = fmt.Errorf("kind not found in group version resources")

	apiResourceCache = sync.Map{}
)

func DiscoverGVR(gvr schema.GroupVersionResource) (*metav1.APIResource, error) {
	val, ok := apiResourceCache.Load(gvr)
	if ok {
		return val.(*metav1.APIResource), nil
	}

	genericClient := client.GetGenericClient()
	if genericClient == nil {
		return nil, fmt.Errorf("no discovery client found")
	}
	discoveryClient := genericClient.DiscoveryClient

	err := retry.OnError(retry.DefaultBackoff, func(err error) bool { return true }, func() error {
		resourceList, err := discoveryClient.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
		if err != nil {
			return err
		}
		for _, r := range resourceList.APIResources {
			if r.Name != gvr.Resource {
				continue
			}
			clone := r.DeepCopy()
			clone.Group = gvr.Group
			clone.Version = gvr.Version
			apiResourceCache.Store(gvr, clone)
			return nil
		}
		return errKindNotFound
	})

	if err != nil {
		if err == errKindNotFound {
			return nil, errors.NewNotFound(gvr.GroupResource(), "resource not found from discovery")
		}

		// This might be caused by abnormal apiserver or etcd, ignore it
		klog.Errorf("Failed to find resources in group version %s: %v", gvr.GroupVersion().String(), err)
		return nil, err
	}

	val, _ = apiResourceCache.Load(gvr)
	return val.(*metav1.APIResource), nil
}
