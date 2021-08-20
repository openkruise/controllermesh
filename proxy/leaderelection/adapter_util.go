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

package leaderelection

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type adapter interface {
	DecodeFrom(body io.ReadCloser) error
	GetHoldIdentity() (string, bool)
	GetName() string
	SetName(name string)
	EncodeInto(*http.Request)
}

type objectAdapter struct {
	runtimeObj runtime.Object
	metaObj    metav1.Object
}

func newObjectAdapter(obj runtime.Object) *objectAdapter {
	return &objectAdapter{runtimeObj: obj, metaObj: obj.(metav1.Object)}
}

func (oa *objectAdapter) DecodeFrom(body io.ReadCloser) error {
	return decodeObject(body, oa.runtimeObj)
}

func (oa *objectAdapter) GetHoldIdentity() (string, bool) {
	recordStr, ok := oa.metaObj.GetAnnotations()[resourcelock.LeaderElectionRecordAnnotationKey]
	if !ok {
		return "", false
	}
	lr := resourcelock.LeaderElectionRecord{}
	if err := json.Unmarshal([]byte(recordStr), &lr); err != nil {
		return "", true
	}
	return lr.HolderIdentity, true
}

func (oa *objectAdapter) GetName() string {
	return oa.metaObj.GetName()
}

func (oa *objectAdapter) SetName(name string) {
	oa.metaObj.SetName(name)
}

func (oa *objectAdapter) EncodeInto(r *http.Request) {
	var length int
	r.Body, length = encodeObject(oa.runtimeObj)
	r.Header.Set("Content-Length", strconv.Itoa(length))
	r.ContentLength = int64(length)
}

type leaseAdapter struct {
	lease *coordinationv1.Lease
}

func newLeaseAdapter() *leaseAdapter {
	return &leaseAdapter{lease: &coordinationv1.Lease{}}
}

func (la *leaseAdapter) DecodeFrom(body io.ReadCloser) error {
	return decodeObject(body, la.lease)
}

func (la *leaseAdapter) GetHoldIdentity() (string, bool) {
	if la.lease.Spec.HolderIdentity == nil {
		return "", false
	}
	return *la.lease.Spec.HolderIdentity, true
}

func (la *leaseAdapter) GetName() string {
	return la.lease.Name
}

func (la *leaseAdapter) SetName(name string) {
	la.lease.Name = name
}

func (la *leaseAdapter) EncodeInto(r *http.Request) {
	var length int
	r.Body, length = encodeObject(la.lease)
	r.Header.Set("Content-Length", strconv.Itoa(length))
	r.ContentLength = int64(length)
}

func decodeObject(body io.ReadCloser, obj runtime.Object) error {
	if body == nil {
		return fmt.Errorf("body is empty")
	}
	bodyBytes, err := ioutil.ReadAll(body)
	body.Close()
	if err != nil {
		return fmt.Errorf("failed to read the body: %v", err)
	}
	if _, _, err := runtimeSerializer.Decode(bodyBytes, nil, obj); err != nil {
		return fmt.Errorf("unabled to decode the response: %v", err)
	}
	return nil
}

func encodeObject(obj runtime.Object) (io.ReadCloser, int) {
	buf := &bytes.Buffer{}
	_ = runtimeSerializer.Encode(obj, buf)
	return ioutil.NopCloser(buf), buf.Len()
}
