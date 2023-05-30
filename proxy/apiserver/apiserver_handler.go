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
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	"github.com/openkruise/controllermesh/proxy/apiserver/router"
	apiserverrouter "github.com/openkruise/controllermesh/proxy/apiserver/router"
	ctrlmeshfilters "github.com/openkruise/controllermesh/proxy/filters"
	trafficfilter "github.com/openkruise/controllermesh/proxy/filters/traffic"
	leaderelectionproxy "github.com/openkruise/controllermesh/proxy/leaderelection"
	"github.com/openkruise/controllermesh/util"
	utilhttp "github.com/openkruise/controllermesh/util/http"
	"github.com/openkruise/controllermesh/util/pool"
)

var (
	upgradeSubresources = sets.NewString("exec", "attach")
	disableIptables     = os.Getenv(constants.EnvDisableIptables) == "true"
)

type Proxy struct {
	opts           *Options
	servingInfo    *server.SecureServingInfo
	insecureServer *http.Server
	handler        http.Handler
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
		cfg:               opts.Config,
		transport:         tp,
		router:            router.New(opts.SpecManager),
		userAgentOverride: opts.UserAgentOverride,
	}
	if opts.LeaderElectionName != "" {
		inHandler.electionHandler = leaderelectionproxy.New(opts.SpecManager, opts.LeaderElectionName)
	} else {
		klog.Infof("Skip proxy leader election for no leader-election-name set")
	}

	var handler http.Handler = inHandler
	handler = trafficfilter.WithTrafficControl(handler, opts.SpecManager)
	handler = genericfilters.WithWaitGroup(handler, opts.LongRunningFunc, opts.HandlerChainWaitGroup)
	handler = genericapifilters.WithRequestInfo(handler, opts.RequestInfoResolver)
	handler = ctrlmeshfilters.WithPanicRecovery(handler, opts.RequestInfoResolver)

	insecureServer := &http.Server{
		Addr:           net.JoinHostPort(opts.SecureServingOptions.BindAddress.String(), strconv.Itoa(opts.SecureServingOptions.BindPort)),
		Handler:        handler,
		MaxHeaderBytes: 1 << 20,
	}

	return &Proxy{opts: opts, servingInfo: servingInfo, insecureServer: insecureServer, handler: handler}, nil
}

func (p *Proxy) Start(ctx context.Context) (<-chan struct{}, error) {
	if disableIptables {
		stoppedCh := make(chan struct{})
		go func() {
			if err := p.insecureServer.ListenAndServe(); err != nil {
				close(stoppedCh)
			}
		}()
		return stoppedCh, nil
	}
	stopped, err := p.servingInfo.Serve(p.handler, time.Minute, ctx.Done())
	if err != nil {
		return nil, fmt.Errorf("error serve with options %s: %v", util.DumpJSON(p.opts), err)
	}
	return stopped, nil
}

type handler struct {
	cfg               *rest.Config
	transport         http.RoundTripper
	router            apiserverrouter.Router
	electionHandler   leaderelectionproxy.Handler
	userAgentOverride string
}

func (h *handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	h.overrideUserAgent(r)

	requestInfo, ok := apirequest.RequestInfoFrom(r.Context())
	if !ok {
		klog.Errorf("%s %s %s, no request info in context", r.Method, r.Header.Get("Content-Type"), r.URL)
		http.Error(rw, "no request info in context", http.StatusBadRequest)
		return
	}

	if requestInfo.IsResourceRequest && upgradeSubresources.Has(requestInfo.Subresource) {
		h.upgradeProxyHandler(rw, r)
		return
	}

	if h.electionHandler != nil {
		if ok, modifyResponse, err := h.electionHandler.Handle(requestInfo, r); err != nil {
			klog.Errorf("%s %s %s, failed to adapt leader election lock: %v", r.Method, r.Header.Get("Content-Type"), r.URL, err)
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		} else if ok {
			p := h.newProxy(r)
			p.ModifyResponse = modifyResponse
			p.ServeHTTP(rw, r)
			return
		}
	}

	accept, err := h.router.Route(r, requestInfo)
	if err != nil {
		http.Error(rw, err.Msg, err.Code)
		return
	}
	modifyBody := accept.ModifyBody
	var statusCode int
	modifyResponse := func(resp *http.Response) error {
		statusCode = resp.StatusCode
		if accept.ModifyResponse != nil {
			return accept.ModifyResponse(resp)
		}
		return nil
	}

	// no need to log leader election requests
	defer func() {
		if klog.V(4).Enabled() {
			klog.InfoS("PROXY",
				"verb", r.Method,
				"URI", r.RequestURI,
				"latency", time.Since(startTime),
				"userAgent", r.UserAgent(),
				"resp", statusCode,
			)
		}
	}()

	p := h.newProxy(r)
	p.ModifyResponse = modifyResponse
	p.ModifyBody = modifyBody
	p.ServeHTTP(rw, r)
}

func (h *handler) overrideUserAgent(r *http.Request) {
	if len(h.userAgentOverride) == 0 {
		return
	}
	userAgent := h.userAgentOverride
	if strings.HasSuffix(h.userAgentOverride, "/") {
		userAgent = userAgent + r.UserAgent()
	}
	r.Header.Set("User-Agent", userAgent)
}

func (h *handler) newProxy(r *http.Request) *utilhttp.ReverseProxy {
	p := utilhttp.NewSingleHostReverseProxy(h.getURL(r))
	p.Transport = h.transport
	p.FlushInterval = 500 * time.Millisecond
	p.BufferPool = pool.BytesPool
	return p
}

func (h *handler) getURL(r *http.Request) *url.URL {
	u, _ := url.Parse(fmt.Sprintf("https://%s", r.Host))
	if disableIptables {
		u, _ = url.Parse(fmt.Sprintf(h.cfg.Host))
		r.Host = ""
	}
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

	p := proxy.NewUpgradeAwareHandler(h.getURL(r), proxyRoundTripper, true, true, &responder{w: rw})
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
