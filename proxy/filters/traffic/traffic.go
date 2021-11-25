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

package traffic

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/client"
	ctrlmeshv1alphainformers "github.com/openkruise/controllermesh/client/informers/externalversions/ctrlmesh/v1alpha1"
	ctrlmeshv1alpha1listers "github.com/openkruise/controllermesh/client/listers/ctrlmesh/v1alpha1"
	proxyclient "github.com/openkruise/controllermesh/proxy/client"
	"github.com/openkruise/controllermesh/util"
)

var (
	cacheLock              sync.Mutex
	cachedPolicies         []*ctrlmeshv1alpha1.TrafficPolicy
	cacheExpirationTime    time.Time
	defaultCacheExpiration = time.Second * 3
)

func WithTrafficControl(handler http.Handler, proxyClient proxyclient.Client) http.Handler {
	ctx := context.Background()
	clientset := client.GetGenericClient().CtrlmeshClient
	namespace := os.Getenv(constants.EnvPodNamespace)
	informer := ctrlmeshv1alphainformers.NewTrafficPolicyInformer(clientset, namespace, 0, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	go func() {
		informer.Run(ctx.Done())
	}()
	if ok := cache.WaitForCacheSync(ctx.Done(), informer.HasSynced); !ok {
		klog.Fatalf("Error waiting TrafficPolicy informer synced")
	}

	return &trafficControlHandler{
		handler:     handler,
		lister:      ctrlmeshv1alpha1listers.NewTrafficPolicyLister(informer.GetIndexer()).TrafficPolicies(namespace),
		proxyClient: proxyClient,
	}
}

type trafficControlHandler struct {
	handler      http.Handler
	lister       ctrlmeshv1alpha1listers.TrafficPolicyNamespaceLister
	proxyClient  proxyclient.Client
	rateControls sync.Map
}

func (h *trafficControlHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snapshot := h.proxyClient.GetProtoSpecSnapshot()
	protoSpec, _, err := snapshot.AcquireSpec()
	if err != nil {
		h.handler.ServeHTTP(w, r)
		return
	}
	snapshot.ReleaseSpec()
	policies, err := h.getTrafficPolicies(protoSpec.Meta.VAppName, protoSpec.RouteInternal.Subset)
	if err != nil {
		klog.Errorf("Failed to get trafficpolicies from lister: %v", err)
		h.handler.ServeHTTP(w, r)
		return
	}
	if len(policies) == 0 {
		h.handler.ServeHTTP(w, r)
		return
	}
	requestInfo, ok := apirequest.RequestInfoFrom(r.Context())
	if !ok {
		handleError(w, r, fmt.Errorf("no RequestInfo found in context, handler chain must be wrong"))
		return
	}

	wrapper := &responseWriterWrapper{ResponseWriter: w}
	rateHashes := sets.NewString()
	for _, policy := range policies {
		if policy.Spec.CircuitBreaking.APIServer != nil {
			if matchesAPIServerRules(requestInfo, policy.Spec.CircuitBreaking.APIServer) {
				http.Error(w, fmt.Sprintf("Circuit breaking by TrafficPolicy %s", policy.Name), http.StatusExpectationFailed)
				return
			}
		}
		if policy.Spec.RateLimiting != nil {
			for i := range policy.Spec.RateLimiting.RatePolicies {
				ratePolicy := &policy.Spec.RateLimiting.RatePolicies[i]
				if !matchesAPIServerRules(requestInfo, &ratePolicy.Rules) {
					continue
				}
				hash := hashRatePolicy(policy.Name, ratePolicy)
				if rateHashes.Has(hash) {
					continue
				}
				rateHashes.Insert(hash)
				val, _ := h.rateControls.LoadOrStore(hash, &rateControl{name: policy.Name, policy: ratePolicy})
				deferFunc, delay, err := val.(*rateControl).allow(requestInfo)
				if deferFunc != nil {
					defer deferFunc(atomic.LoadInt32(&wrapper.status))
				}
				if err != nil {
					tooManyRequests(w, delay, err.Error())
					return
				}
			}
		}
	}
	h.handler.ServeHTTP(wrapper, r)
}

func (h *trafficControlHandler) getTrafficPolicies(vAppName, subset string) (ret []*ctrlmeshv1alpha1.TrafficPolicy, err error) {
	cacheLock.Lock()
	defer cacheLock.Unlock()
	now := time.Now()
	if now.Before(cacheExpirationTime) {
		return cachedPolicies, nil
	}

	policies, err := h.lister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for i := range policies {
		for _, target := range policies[i].Spec.TargetVirtualApps {
			if target.Name != vAppName {
				continue
			}
			if len(target.SpecificSubsets) > 0 && !containsString(subset, target.SpecificSubsets, "*") {
				continue
			}
			ret = append(ret, policies[i])
			break
		}
	}
	cachedPolicies = ret
	cacheExpirationTime = now.Add(defaultCacheExpiration)
	return
}

func hashRatePolicy(name string, rp *ctrlmeshv1alpha1.TrafficRateLimitingPolicy) string {
	return name + "--" + util.GetMD5Hash(util.DumpJSON(rp))
}

func handleError(w http.ResponseWriter, r *http.Request, err error) {
	errorMsg := fmt.Sprintf("Internal Server Error: %#v", r.RequestURI)
	http.Error(w, errorMsg, http.StatusInternalServerError)
	klog.Errorf(err.Error())
}

func tooManyRequests(w http.ResponseWriter, delaySeconds int, msg string) {
	if delaySeconds <= 1 {
		delaySeconds = 1
	}
	// Return a 429 status indicating "Too Many Requests"
	w.Header().Set("Retry-After", strconv.Itoa(delaySeconds))
	http.Error(w, msg, http.StatusTooManyRequests)
}

type responseWriterWrapper struct {
	http.ResponseWriter

	statusRecorded bool
	status         int32
}

// Header implements http.ResponseWriter.
func (r *responseWriterWrapper) Header() http.Header {
	return r.ResponseWriter.Header()
}

// Write implements http.ResponseWriter.
func (r *responseWriterWrapper) Write(b []byte) (int, error) {
	if !r.statusRecorded {
		r.recordStatus(http.StatusOK) // Default if WriteHeader hasn't been called
	}
	return r.ResponseWriter.Write(b)
}

// WriteHeader implements http.ResponseWriter.
func (r *responseWriterWrapper) WriteHeader(status int) {
	r.recordStatus(status)
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseWriterWrapper) recordStatus(status int) {
	atomic.StoreInt32(&r.status, int32(status))
	r.statusRecorded = true
}

// Flush implements http.Flusher even if the underlying http.Writer doesn't implement it.
// Flush is used for streaming purposes and allows to flush buffered data to the client.
func (r *responseWriterWrapper) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	} else if klog.V(2).Enabled() {
		klog.InfoDepth(1, fmt.Sprintf("Unable to convert %T into http.Flusher", r.ResponseWriter))
	}
}

// Hijack implements http.Hijacker.
func (r *responseWriterWrapper) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.ResponseWriter.(http.Hijacker).Hijack()
}

// CloseNotify implements http.CloseNotifier
func (r *responseWriterWrapper) CloseNotify() <-chan bool {
	//lint:ignore SA1019 There are places in the code base requiring the CloseNotifier interface to be implemented.
	return r.ResponseWriter.(http.CloseNotifier).CloseNotify()
}
