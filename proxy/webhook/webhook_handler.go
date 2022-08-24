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

package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/openkruise/controllermesh/proxy/protomanager"
	webhookrouter "github.com/openkruise/controllermesh/proxy/webhook/router"
	"github.com/openkruise/controllermesh/util"
	"github.com/openkruise/controllermesh/util/certwatcher"
	utilhttp "github.com/openkruise/controllermesh/util/http"
	"github.com/openkruise/controllermesh/util/pool"
)

var admissionScheme = runtime.NewScheme()
var admissionCodecs = serializer.NewCodecFactory(admissionScheme)

func init() {
	utilruntime.Must(admissionv1.AddToScheme(admissionScheme))
}

var (
	tlsFileNames = [][]string{
		{"tls.key", "tls.crt"},
		{"key.pem", "cert.pem"},
	}

	defaultDial = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
)

type Options struct {
	CertDir     string
	BindPort    int
	WebhookPort int
	SpecManager *protomanager.SpecManager
}

type Proxy struct {
	opts    *Options
	started bool
	mu      sync.Mutex
}

func NewProxy(opts *Options) *Proxy {
	return &Proxy{opts: opts}
}

func (p *Proxy) Start(ctx context.Context) (<-chan struct{}, error) {
	stopped := make(chan struct{})
	go func() {
		var keyFile, certFile string
		var err error
		for {
			select {
			case <-ctx.Done():
				close(stopped)
				return
			default:
			}
			keyFile, certFile, err = p.ensureCerts()
			if err == nil {
				break
			}
			klog.Warningf("Webhook proxy started and waiting for certs ready: %v", err)
			time.Sleep(time.Second * 3)
		}

		for {
			select {
			case <-ctx.Done():
				close(stopped)
				return
			default:
			}
			err = util.CheckTCPPortOpened("127.0.0.1", p.opts.WebhookPort)
			if err == nil {
				break
			}
			klog.Warningf("Webhook proxy started and waiting for webhook ready: %v", err)
			time.Sleep(time.Second * 3)
		}

		certWatcher, err := certwatcher.New(certFile, keyFile)
		if err != nil {
			klog.Fatalf("Webhook proxy failed to watch certs %s, %s: %v", certFile, keyFile, err)
		}

		go func() {
			if err := certWatcher.Start(ctx); err != nil {
				klog.Fatalf("Webhook proxy failed to start watching certs %s, %s: %v", certFile, keyFile, err)
			}
		}()

		addr := fmt.Sprintf(":%d", p.opts.BindPort)
		listener, err := tls.Listen("tcp", addr, &tls.Config{NextProtos: []string{"h2"}, GetCertificate: certWatcher.GetCertificate})
		if err != nil {
			klog.Fatalf("Webhook proxy failed to listen on %s : %v", addr, err)
		}

		cache := &transportCache{webhookPort: p.opts.WebhookPort}
		// preparing 127.0.0.1 into cache
		_ = cache.getTripper("127.0.0.1")
		srv := &http.Server{
			Handler: &handler{
				router: webhookrouter.New(p.opts.SpecManager),
				cache:  cache,
			},
		}

		klog.Infof("Webhook proxy serving on %s, redirect to webhook port %d", addr, p.opts.WebhookPort)
		p.mu.Lock()
		p.started = true
		p.mu.Unlock()

		go func() {
			defer close(stopped)
			<-ctx.Done()
			_ = srv.Shutdown(context.Background())
		}()

		err = srv.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Webhook proxy failed to serve on %s : %v", addr, err)
		}
	}()
	return stopped, nil
}

func (p *Proxy) HealthFunc(_ *http.Request) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return fmt.Errorf("not started")
	}
	return nil
}

func (p *Proxy) ensureCerts() (keyFile, certFile string, err error) {
	if _, err := os.Stat(p.opts.CertDir); err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("cert dir %s not found", p.opts.CertDir)
		}
		return "", "", fmt.Errorf("state cert dir %s error: %v", p.opts.CertDir, err)
	}

	for _, names := range tlsFileNames {
		key := path.Join(p.opts.CertDir, names[0])
		cert := path.Join(p.opts.CertDir, names[1])
		if _, err := os.Stat(key); err != nil {
			continue
		}
		if _, err := os.Stat(cert); err != nil {
			continue
		}
		return key, cert, nil
	}

	return "", "", fmt.Errorf("not found cert files %v in directory %s", tlsFileNames, p.opts.CertDir)
}

type handler struct {
	router webhookrouter.Router
	cache  *transportCache
}

func (h *handler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		writeResponse(rw, admission.Errored(http.StatusBadRequest, fmt.Errorf("request body is empty")))
		return
	}
	body, err := ioutil.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeResponse(rw, admission.Errored(http.StatusBadRequest, fmt.Errorf("unable to read the body from the incoming request: %v", err)))
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		writeResponse(rw, admission.Errored(http.StatusBadRequest, fmt.Errorf("contentType=%s, expected application/json", contentType)))
		return
	}

	ar := admissionv1.AdmissionReview{}
	if _, _, err := admissionCodecs.UniversalDeserializer().Decode(body, nil, &ar); err != nil {
		writeResponse(rw, admission.Errored(http.StatusBadRequest, fmt.Errorf("unabled to decode the request: %v", err)))
		return
	} else if ar.Request == nil {
		writeResponse(rw, admission.Errored(http.StatusBadRequest, fmt.Errorf("request in admission review is empty")))
		return
	}

	klog.V(4).Infof("%s %s %s to %s", r.Method, r.Header.Get("Content-Type"), r.URL, r.Host)

	accept, redirect, ignore, routeErr := h.router.Route(ar.Request)
	if routeErr != nil {
		writeResponse(rw, admission.Errored(int32(routeErr.Code), fmt.Errorf(routeErr.Msg)))
		return
	}

	var tripper http.RoundTripper
	switch {
	case ignore != nil:
		writeResponse(rw, admission.Allowed(""))
		return
	case redirect != nil:
		tripper = h.cache.getOneTripper(redirect.Hosts)
	case accept != nil:
		tripper = h.cache.getTripper("127.0.0.1")
	}

	// set a new body, which will simulate the same data we read:
	r.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	u, _ := url.Parse(fmt.Sprintf("https://%s", r.Host))
	p := utilhttp.NewSingleHostReverseProxy(u)
	p.BufferPool = pool.BytesPool
	p.Transport = tripper
	p.ServeHTTP(rw, r)
}

func writeResponse(w io.Writer, response admission.Response) {
	encoder := json.NewEncoder(w)
	responseAdmissionReview := admissionv1.AdmissionReview{
		Response: &response.AdmissionResponse,
	}
	err := encoder.Encode(responseAdmissionReview)
	if err != nil {
		writeResponse(w, admission.Errored(http.StatusInternalServerError, err))
	}
}

type transportCache struct {
	sync.Mutex
	cache       map[string]http.RoundTripper
	webhookPort int
}

func (tc *transportCache) getTripper(host string) http.RoundTripper {
	return tc.getOneTripper([]string{host})
}

func (tc *transportCache) getOneTripper(hosts []string) http.RoundTripper {
	tc.Lock()
	defer tc.Unlock()
	for _, host := range hosts {
		if rt, ok := tc.cache[host]; ok {
			return rt
		}
	}
	if tc.cache == nil {
		tc.cache = make(map[string]http.RoundTripper, 10)
	}
	host := hosts[0]
	redirectHost := fmt.Sprintf("%s:%d", host, tc.webhookPort)
	cfg := &transport.Config{
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return defaultDial(ctx, network, redirectHost)
		},
		TLS: transport.TLSConfig{Insecure: true},
	}
	tripper, err := transport.New(cfg)
	if err != nil {
		klog.Fatalf("Webhook proxy failed to new transport %s: %v", redirectHost, err)
	}
	tc.cache[host] = tripper
	return tripper
}
