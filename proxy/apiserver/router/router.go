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

package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog/v2"

	ctrlmeshproto "github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	proxyclient "github.com/openkruise/controllermesh/proxy/client"
	"github.com/openkruise/controllermesh/util"
	utildiscovery "github.com/openkruise/controllermesh/util/discovery"
)

type Router interface {
	Route(*http.Request, *request.RequestInfo) (*RouteAccept, *Error)
}

type RouteAccept struct {
	ModifyResponse func(response *http.Response) error
	ModifyBody     func(*http.Response) io.Reader
}

type Error struct {
	Code int
	Msg  string
}

type router struct {
	proxyClient proxyclient.Client
}

func New(c proxyclient.Client) Router {
	return &router{proxyClient: c}
}

func (r *router) Route(httpReq *http.Request, reqInfo *request.RequestInfo) (*RouteAccept, *Error) {
	if !reqInfo.IsResourceRequest {
		return &RouteAccept{}, nil
	}
	_, protoRoute, _, _, _, _ := r.proxyClient.GetProtoSpec()
	if protoRoute.IsDefaultEmpty() {
		return &RouteAccept{}, nil
	}

	gvr := schema.GroupVersionResource{Group: reqInfo.APIGroup, Version: reqInfo.APIVersion, Resource: reqInfo.Resource}
	apiResource, err := utildiscovery.DiscoverGVR(gvr)
	if err != nil {
		// let requests with non-existing resources go
		if errors.IsNotFound(err) {
			return &RouteAccept{}, nil
		}
		return nil, &Error{Code: http.StatusNotFound, Msg: fmt.Sprintf("failed to get gvr %v from discovery: %v", gvr, err)}
	}

	conf := &config{
		httpReq:     httpReq,
		reqInfo:     reqInfo,
		apiResource: apiResource,
		protoRoute:  protoRoute,
	}
	switch reqInfo.Verb {
	case "list":
		handler := &listHandler{config: *conf}
		return &RouteAccept{ModifyResponse: handler.handle}, nil

	case "watch":
		handler := &watchHandler{config: *conf}
		return &RouteAccept{ModifyBody: handler.handle}, nil

	default:
	}

	if !protoRoute.IsNamespaceMatch(reqInfo.Namespace) {
		return nil, &Error{Code: http.StatusForbidden, Msg: "not match subset rules"}
	}

	return &RouteAccept{}, nil
}

type config struct {
	httpReq     *http.Request
	reqInfo     *request.RequestInfo
	apiResource *metav1.APIResource
	protoRoute  *ctrlmeshproto.InternalRoute
}

type listHandler struct {
	config
}

func (h *listHandler) handle(resp *http.Response) error {
	if resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusPartialContent {
		return nil
	}

	respSerializer, err := newResponseSerializer(resp, h.apiResource, h.reqInfo, false)
	if err != nil {
		return fmt.Errorf("new response serializer for list response error: %v", err)
	}

	obj, err := respSerializer.DecodeList(h.httpReq.Context())
	if err != nil {
		respSerializer.Release()
		klog.Errorf("Failed to decode list response: %v", err)
		return err
	}

	if apimeta.IsListType(obj) {
		if err := h.filterItems(obj); err != nil {
			klog.Warningf("Failed to handle %v filter error: %v, object: %v", util.DumpJSON(h.reqInfo), err, util.DumpJSON(obj))
		}
	} else {
		klog.Warningf("Handle %v response is not list", util.DumpJSON(h.reqInfo))
	}

	readerCloser, length, err := respSerializer.EncodeList(obj)
	if err != nil {
		respSerializer.Release()
		klog.Errorf("Failed to encode list response: %v", err)
		return err
	}
	resp.Body = readerCloser
	resp.Header.Set("Content-Length", strconv.Itoa(length))
	resp.ContentLength = int64(length)
	return nil
}

func (h *listHandler) filterItems(list runtime.Object) error {
	if unstructuredObj, ok := list.(*unstructured.Unstructured); ok {
		itemsField, ok := unstructuredObj.Object["items"]
		if !ok {
			return fmt.Errorf("object is unstructured but not list")
		}
		items, ok := itemsField.([]interface{})
		if !ok {
			return fmt.Errorf("object is unstructured but not list")
		}
		var newItems []interface{}
		for _, item := range items {
			child, ok := item.(map[string]interface{})
			if !ok {
				return fmt.Errorf("items member is not an object: %T", child)
			}
			childObj := &unstructured.Unstructured{Object: child}
			if h.protoRoute.IsNamespaceMatch(childObj.GetNamespace()) {
				newItems = append(newItems, item)
			} else {
				klog.V(5).Infof("Filter out list item %s/%s for %v", childObj.GetNamespace(), childObj.GetName(), util.DumpJSON(h.reqInfo))
			}
		}
		unstructuredObj.Object["items"] = newItems
		return nil
	}

	objs, err := apimeta.ExtractList(list)
	if err != nil {
		return err
	}

	var newObjs []runtime.Object
	for i := range objs {
		o := objs[i]
		meta, err := apimeta.Accessor(o)
		if err != nil {
			newObjs = append(newObjs, o)
			continue
		}
		if h.protoRoute.IsNamespaceMatch(meta.GetNamespace()) {
			newObjs = append(newObjs, o)
		} else {
			klog.V(5).Infof("Filter out list item %s/%s for %v", meta.GetNamespace(), meta.GetName(), util.DumpJSON(h.reqInfo))
		}
	}

	return apimeta.SetList(list, newObjs)
}

type watchHandler struct {
	config
	respSerializer *responseSerializer
	remaining      []byte
}

func (h *watchHandler) handle(resp *http.Response) io.Reader {
	if resp.StatusCode != http.StatusOK {
		return resp.Body
	}
	var err error
	h.respSerializer, err = newResponseSerializer(resp, h.apiResource, h.reqInfo, true)
	if err != nil {
		klog.Errorf("Failed to new response serializer for list response error: %v", err)
		return resp.Body
	}
	return h
}

func (h *watchHandler) Read(p []byte) (int, error) {
	// Return whatever remaining data exists from an in progress frame
	if n := len(h.remaining); n > 0 {
		if n <= len(p) {
			p = append(p[0:0], h.remaining...)
			h.remaining = nil
			return n, nil
		}

		n = len(p)
		p = append(p[0:0], h.remaining[:n]...)
		h.remaining = h.remaining[n:]
		return n, nil
	}

	body, err := h.read()
	if err != nil {
		h.respSerializer.Release()
		return 0, err
	}

	n := len(p)
	// If capacity of data is less than length of the message, decoder will allocate a new slice
	// and set m to it, which means we need to copy the partial result back into data and preserve
	// the remaining result for subsequent reads.
	if len(body) > n {
		p = append(p[0:0], body[:n]...)
		h.remaining = body[n:]
		return n, nil
	}
	p = append(p[0:0], body...)
	return len(body), nil
}

func (h *watchHandler) read() ([]byte, error) {
	var err error
	var e *metav1.WatchEvent
	var obj runtime.Object
	triggerChan := make(chan struct{}, 1)
	go func() {
		e, obj, err = h.respSerializer.DecodeWatch()
		close(triggerChan)
	}()

	ctx := h.httpReq.Context()
	select {
	case <-ctx.Done():
		return nil, context.Canceled
	case <-triggerChan:
	}

	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		klog.Errorf("Failed to decode watch response: %v", err)
		return nil, err
	}

	// obj will be nil if the event type is BOOKMARK or ERROR
	if obj != nil {
		h.filterEvent(e, obj)
	}

	body, err := h.respSerializer.EncodeWatch(e)
	if err != nil {
		klog.Errorf("Failed to encode watch response: %v", err)
		return nil, err
	}
	return body, nil
}

func (h *watchHandler) filterEvent(e *metav1.WatchEvent, obj runtime.Object) {
	meta, err := apimeta.Accessor(obj)
	if err != nil {
		return
	}
	if !h.protoRoute.IsNamespaceMatch(meta.GetNamespace()) {
		e.Type = string(watch.Bookmark)
		klog.V(5).Infof("Filter out watch item %s/%s for %v", meta.GetNamespace(), meta.GetName(), util.DumpJSON(h.reqInfo))
	}
}
