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

package apiserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"

	"github.com/openkruise/controllermesh/client"
	"github.com/openkruise/controllermesh/proxy/apiserver/router"
	apiserverrouter "github.com/openkruise/controllermesh/proxy/apiserver/router"
	leaderelectionproxy "github.com/openkruise/controllermesh/proxy/leaderelection"
	"github.com/openkruise/controllermesh/util"
	utilhttp "github.com/openkruise/controllermesh/util/http"
	"github.com/openkruise/controllermesh/util/pool"
)

var (
	upgradeSubresources = sets.NewString("exec", "attach")
)

type Proxy struct {
	opts        *Options
	servingInfo *server.SecureServingInfo
	handler     http.Handler
}

func NewProxy(opts *Options) (*Proxy, error) {
	var servingInfo *server.SecureServingInfo
	if err := opts.ApplyTo(&servingInfo); err != nil {
		return nil, fmt.Errorf("error apply options %s: %v", util.DumpJSON(opts), err)
	}

	tp, err := rest.TransportFor(opts.Config)
	if err != nil {
		return nil, fmt.Errorf("error get transport for config %s: %v", util.DumpJSON(opts.Config), err)
	}

	inHandler := &handler{
		cfg:       opts.Config,
		transport: tp,
		router:    router.New(opts.ProxyClient),
	}
	if opts.LeaderElectionName != "" {
		inHandler.electionHandler = leaderelectionproxy.New(client.GetGenericClient().KubeClient, opts.ProxyClient, opts.LeaderElectionName)
	} else {
		klog.Infof("Skip proxy leader election for no leader-election-name set")
	}

	var handler http.Handler = inHandler
	handler = genericfilters.WithWaitGroup(handler, opts.LongRunningFunc, opts.HandlerChainWaitGroup)
	handler = genericapifilters.WithRequestInfo(handler, opts.RequestInfoResolver)
	handler = genericfilters.WithPanicRecovery(handler, opts.RequestInfoResolver)

	return &Proxy{opts: opts, servingInfo: servingInfo, handler: handler}, nil
}

func (p *Proxy) Start(ctx context.Context) (<-chan struct{}, error) {
	stopped, err := p.servingInfo.Serve(p.handler, time.Minute, ctx.Done())
	if err != nil {
		return nil, fmt.Errorf("error serve with options %s: %v", util.DumpJSON(p.opts), err)
	}
	return stopped, nil
}

type handler struct {
	cfg             *rest.Config
	transport       http.RoundTripper
	router          apiserverrouter.Router
	electionHandler leaderelectionproxy.Handler
}

func (h *handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	requestInfo, ok := apirequest.RequestInfoFrom(r.Context())
	if !ok {
		klog.Errorf("%s %s %s, no request info in context", r.Method, r.Header.Get("Content-Type"), r.URL)
		http.Error(rw, "no request info in context", http.StatusBadRequest)
		return
	}
	klog.V(5).Infof("%s %s %s request info %s", r.Method, r.Header.Get("Content-Type"), r.URL, util.DumpJSON(requestInfo))

	var modifyResponse func(*http.Response) error
	var modifyBody func(*http.Response) io.Reader
	if h.electionHandler != nil {
		if ok, wrapErr := h.electionHandler.Handle(requestInfo, r); wrapErr != nil {
			klog.Errorf("%s %s %s, failed to adapt leader election lock: %v", r.Method, r.Header.Get("Content-Type"), r.URL, wrapErr)
			http.Error(rw, wrapErr.Err.Error(), wrapErr.Code)
			return
		} else if ok {
			p := h.newProxy(r)
			p.ServeHTTP(rw, r)
			return
		}

		accept, err := h.router.Route(r, requestInfo)
		if err != nil {
			http.Error(rw, err.Msg, err.Code)
			return
		}
		modifyResponse = accept.ModifyResponse
		modifyBody = accept.ModifyBody
	}

	if requestInfo.IsResourceRequest && upgradeSubresources.Has(requestInfo.Subresource) {
		h.upgradeProxyHandler(rw, r)
		return
	}

	p := h.newProxy(r)
	p.ModifyResponse = modifyResponse
	p.ModifyBody = modifyBody
	p.ServeHTTP(rw, r)
}

func (h *handler) newProxy(r *http.Request) *utilhttp.ReverseProxy {
	p := utilhttp.NewSingleHostReverseProxy(getURL(r))
	p.Transport = h.transport
	p.FlushInterval = 500 * time.Millisecond
	p.BufferPool = pool.BytesPool
	return p
}

func getURL(r *http.Request) *url.URL {
	u, _ := url.Parse(fmt.Sprintf("https://%s", r.Host))
	return u
}

func (h *handler) upgradeProxyHandler(rw http.ResponseWriter, r *http.Request) {
	tlsConfig, err := rest.TLSConfigFor(h.cfg)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	upgradeRoundTripper := spdy.NewRoundTripper(tlsConfig, true, true)
	wrappedRT, err := rest.HTTPWrappersForConfig(h.cfg, upgradeRoundTripper)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	proxyRoundTripper := transport.NewAuthProxyRoundTripper(user.APIServerUser, []string{user.SystemPrivilegedGroup}, nil, wrappedRT)

	p := proxy.NewUpgradeAwareHandler(getURL(r), proxyRoundTripper, true, true, &responder{w: rw})
	p.ServeHTTP(rw, r)
}

// responder implements ErrorResponder for assisting a connector in writing objects or errors.
type responder struct {
	w http.ResponseWriter
}

// TODO: this should properly handle content type negotiation
// if the caller asked for protobuf and you write JSON bad things happen.
func (r *responder) Object(statusCode int, obj runtime.Object) {
	responsewriters.WriteRawJSON(statusCode, obj, r.w)
}

func (r *responder) Error(_ http.ResponseWriter, _ *http.Request, err error) {
	http.Error(r.w, err.Error(), http.StatusInternalServerError)
}
