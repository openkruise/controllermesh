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
// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeVirtualApps implements VirtualAppInterface
type FakeVirtualApps struct {
	Fake *FakeCtrlmeshV1alpha1
	ns   string
}

var virtualappsResource = schema.GroupVersionResource{Group: "ctrlmesh.kruise.io", Version: "v1alpha1", Resource: "virtualapps"}

var virtualappsKind = schema.GroupVersionKind{Group: "ctrlmesh.kruise.io", Version: "v1alpha1", Kind: "VirtualApp"}

// Get takes name of the virtualApp, and returns the corresponding virtualApp object, and an error if there is any.
func (c *FakeVirtualApps) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.VirtualApp, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(virtualappsResource, c.ns, name), &v1alpha1.VirtualApp{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.VirtualApp), err
}

// List takes label and field selectors, and returns the list of VirtualApps that match those selectors.
func (c *FakeVirtualApps) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.VirtualAppList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(virtualappsResource, virtualappsKind, c.ns, opts), &v1alpha1.VirtualAppList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1alpha1.VirtualAppList{ListMeta: obj.(*v1alpha1.VirtualAppList).ListMeta}
	for _, item := range obj.(*v1alpha1.VirtualAppList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested virtualApps.
func (c *FakeVirtualApps) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(virtualappsResource, c.ns, opts))

}

// Create takes the representation of a virtualApp and creates it.  Returns the server's representation of the virtualApp, and an error, if there is any.
func (c *FakeVirtualApps) Create(ctx context.Context, virtualApp *v1alpha1.VirtualApp, opts v1.CreateOptions) (result *v1alpha1.VirtualApp, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(virtualappsResource, c.ns, virtualApp), &v1alpha1.VirtualApp{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.VirtualApp), err
}

// Update takes the representation of a virtualApp and updates it. Returns the server's representation of the virtualApp, and an error, if there is any.
func (c *FakeVirtualApps) Update(ctx context.Context, virtualApp *v1alpha1.VirtualApp, opts v1.UpdateOptions) (result *v1alpha1.VirtualApp, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(virtualappsResource, c.ns, virtualApp), &v1alpha1.VirtualApp{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.VirtualApp), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeVirtualApps) UpdateStatus(ctx context.Context, virtualApp *v1alpha1.VirtualApp, opts v1.UpdateOptions) (*v1alpha1.VirtualApp, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(virtualappsResource, "status", c.ns, virtualApp), &v1alpha1.VirtualApp{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.VirtualApp), err
}

// Delete takes name of the virtualApp and deletes it. Returns an error if one occurs.
func (c *FakeVirtualApps) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteAction(virtualappsResource, c.ns, name), &v1alpha1.VirtualApp{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeVirtualApps) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(virtualappsResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &v1alpha1.VirtualAppList{})
	return err
}

// Patch applies the patch and returns the patched virtualApp.
func (c *FakeVirtualApps) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.VirtualApp, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(virtualappsResource, c.ns, name, pt, data, subresources...), &v1alpha1.VirtualApp{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.VirtualApp), err
}
