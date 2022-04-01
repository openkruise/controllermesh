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
	"net/url"
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

	"github.com/openkruise/controllermesh/apis/ctrlmesh/proto"
	proxyclient "github.com/openkruise/controllermesh/proxy/client"
	"github.com/openkruise/controllermesh/util"
	utildiscovery "github.com/openkruise/controllermesh/util/discovery"
	httputil "github.com/openkruise/controllermesh/util/http"
)

const (
	labelSelectorKey = "labelSelector"
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

	snapshot := r.proxyClient.GetProtoSpecSnapshot()
	protoSpec, _, err := snapshot.AcquireSpec()
	if err != nil {
		return nil, &Error{Code: http.StatusExpectationFailed, Msg: "route snapshot exceeded changed"}
	}
	defer snapshot.ReleaseSpec()
	if protoSpec.RouteInternal.IsDefaultAndEmpty() {
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

	switch reqInfo.Verb {
	case "list", "watch":
		conf := &config{
			httpReq:       httpReq,
			reqInfo:       reqInfo,
			groupResource: gvr.GroupResource(),
			apiResource:   apiResource,
			snapshot:      snapshot,
		}
		if err = r.injectSelector(protoSpec.RouteInternal, httpReq, &gvr); err != nil {
			return nil, &Error{Code: http.StatusInternalServerError, Msg: fmt.Sprintf("failed to inject lebal selector with gvr %v: %v", gvr, err)}
		}
		if reqInfo.Verb == "list" {
			handler := &listHandler{config: *conf}
			return &RouteAccept{ModifyResponse: handler.handle}, nil
		}
		handler := &watchHandler{config: *conf}
		return &RouteAccept{ModifyBody: handler.handle}, nil
	default:
	}

	if !protoSpec.RouteInternal.IsNamespaceMatch(reqInfo.Namespace, gvr.GroupResource()) {
		return nil, &Error{Code: http.StatusForbidden, Msg: "not match subset rules"}
	}

	return &RouteAccept{}, nil
}

func (r *router) injectSelector(route *proto.InternalRoute, httpReq *http.Request, gvr *schema.GroupVersionResource) error {
	var subset *proto.InternalSubsetLimit
	for _, sub := range route.SubsetLimits {
		if sub.Subset == route.Subset {
			subset = sub
			break
		}
	}
	if subset == nil {
		return fmt.Errorf("can not find subset rule %s", route.Subset)
	}
	lbSelector := determineObjectSelector(route.GlobalLimits, gvr)
	if lbSelector.Size() == 0 {
		return nil
	}
	raw, err := httputil.ParseRawQuery(httpReq.URL.RawQuery)
	if err != nil {
		return err
	}
	var oldLabelSelector *metav1.LabelSelector
	if oldSelector, ok := raw[labelSelectorKey]; ok {
		oldSelector, err = url.QueryUnescape(oldSelector)
		if err != nil {
			return err
		}
		oldLabelSelector, err = metav1.ParseToLabelSelector(oldSelector)
		if err != nil {
			return err
		}
	}
	lbSelector = mergeSelector(oldLabelSelector, lbSelector)
	selector, err := metav1.LabelSelectorAsSelector(lbSelector)
	if err != nil {
		return err
	}
	raw[labelSelectorKey] = url.QueryEscape(selector.String())
	httpReq.Header.Add("OLD-RAW-QUERY", httpReq.URL.RawQuery)
	oldURL := httpReq.URL.String()
	httpReq.URL.RawQuery = httputil.MarshalRawQuery(raw)
	klog.Infof("Injected object selector in request, %s -> %s", oldURL, httpReq.URL.String())
	return nil
}

func determineObjectSelector(rules []*proto.InternalMatchLimitRule, gvr *schema.GroupVersionResource) *metav1.LabelSelector {
	for _, limiter := range rules {
		if limiter.ObjectSelector == nil {
			continue
		}
		if proto.IsGRMatchedAPIResources(gvr.GroupResource(), limiter.Resources) {
			return limiter.ObjectSelector
		}
	}
	return nil
}

func mergeSelector(selA *metav1.LabelSelector, selB *metav1.LabelSelector) *metav1.LabelSelector {
	if selB == nil {
		return selA.DeepCopy()
	}
	if selA == nil {
		return selB.DeepCopy()
	}
	result := selA.DeepCopy()
	if selB.MatchExpressions != nil {
		if result.MatchExpressions == nil {
			result.MatchExpressions = []metav1.LabelSelectorRequirement{}
		}
		result.MatchExpressions = append(result.MatchExpressions, selB.MatchExpressions...)
	}
	if selB.MatchLabels != nil {
		if result.MatchLabels == nil {
			result.MatchLabels = map[string]string{}
		}
		for key, val := range selB.MatchLabels {
			result.MatchLabels[key] = val
		}
	}
	return result
}

type config struct {
	httpReq       *http.Request
	reqInfo       *request.RequestInfo
	groupResource schema.GroupResource
	apiResource   *metav1.APIResource
	snapshot      *proxyclient.ProtoSpecSnapshot
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
	protoSpec, _, err := h.snapshot.AcquireSpec()
	if err != nil {
		return err
	}
	var allowedNamespaces []string
	var deniedNamespaces []string
	defer func() {
		h.snapshot.RecordNamespace(allowedNamespaces, deniedNamespaces)
		h.snapshot.ReleaseSpec()
	}()
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
			ns := childObj.GetNamespace()
			if protoSpec.RouteInternal.IsNamespaceMatch(ns, h.groupResource) {
				newItems = append(newItems, item)
				if ns != "" {
					allowedNamespaces = append(allowedNamespaces, ns)
				}
			} else {
				klog.V(5).Infof("Filter out list item %s/%s for %v", ns, childObj.GetName(), util.DumpJSON(h.reqInfo))
				if ns != "" {
					deniedNamespaces = append(deniedNamespaces, ns)
				}
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
		ns := meta.GetNamespace()
		if protoSpec.RouteInternal.IsNamespaceMatch(ns, h.groupResource) {
			newObjs = append(newObjs, o)
			if ns != "" {
				allowedNamespaces = append(allowedNamespaces, ns)
			}
		} else {
			klog.V(5).Infof("Filter out list item %s/%s for %v", ns, meta.GetName(), util.DumpJSON(h.reqInfo))
			if ns != "" {
				deniedNamespaces = append(deniedNamespaces, ns)
			}
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
		if err := h.filterEvent(e, obj); err != nil {
			return nil, err
		}
	}

	body, err := h.respSerializer.EncodeWatch(e)
	if err != nil {
		klog.Errorf("Failed to encode watch response: %v", err)
		return nil, err
	}
	return body, nil
}

func (h *watchHandler) filterEvent(e *metav1.WatchEvent, obj runtime.Object) error {
	meta, err := apimeta.Accessor(obj)
	if err != nil {
		return err
	}
	protoSpec, _, err := h.snapshot.AcquireSpec()
	if err != nil {
		return err
	}
	defer h.snapshot.ReleaseSpec()
	ns := meta.GetNamespace()
	if !protoSpec.RouteInternal.IsNamespaceMatch(ns, h.groupResource) {
		e.Type = string(watch.Bookmark)
		klog.V(5).Infof("Filter out watch item %s/%s for %v", ns, meta.GetName(), util.DumpJSON(h.reqInfo))
		h.snapshot.RecordNamespace(nil, []string{ns})
	} else {
		h.snapshot.RecordNamespace([]string{ns}, nil)
	}
	return nil
}
