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

package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	"github.com/openkruise/controllermesh/client"
	apiserverproxy "github.com/openkruise/controllermesh/proxy/apiserver"
	proxyclient "github.com/openkruise/controllermesh/proxy/client"
	"github.com/openkruise/controllermesh/proxy/metrics"
	webhookproxy "github.com/openkruise/controllermesh/proxy/webhook"
	"github.com/openkruise/controllermesh/util"
)

var (
	metricsHealthPort  = flag.Int(constants.ProxyMetricsHealthPortFlag, constants.ProxyMetricsHealthPort, "Port to bind 0.0.0.0 and serve metric endpoint/healthz/pprof.")
	proxyApiserverPort = flag.Int(constants.ProxyApiserverPortFlag, constants.ProxyApiserverPort, "Port to bind localhost and proxy the requests to apiserver.")
	proxyWebhookPort   = flag.Int(constants.ProxyWebhookPortFlag, constants.ProxyWebhookPort, "Port to bind 0.0.0.0 and proxy the requests to webhook.")

	leaderElectionName = flag.String(constants.ProxyLeaderElectionNameFlag, "", "The name of leader election.")
	webhookServePort   = flag.Int(constants.ProxyWebhookServePortFlag, 0, "Port that the real webhook binds, 0 means no proxy for webhook.")
	webhookCertDir     = flag.String(constants.ProxyWebhookCertDirFlag, "", "The directory where the webhook certs generated or mounted.")
)

func main() {
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	if os.Getenv(constants.EnvPodNamespace) == "" || os.Getenv(constants.EnvPodName) == "" {
		klog.Fatalf("Environment %s=%s %s=%s not exist.",
			constants.EnvPodNamespace, os.Getenv(constants.EnvPodNamespace), constants.EnvPodName, os.Getenv(constants.EnvPodName))
	}

	cfg, err := getRestConfig()
	if err != nil {
		klog.Fatalf("Failed to get rest config: %v", err)
	}
	if err := client.NewRegistry(cfg); err != nil {
		klog.Fatalf("Failed to new client registry: %v", err)
	}

	ctx := signals.SetupSignalHandler()
	readyHandler := &healthz.Handler{}
	proxyClient := proxyclient.NewGrpcClient()
	if err := proxyClient.Start(ctx); err != nil {
		klog.Fatalf("Failed to start proxy client: %v", err)
	}

	var stoppedApiserver, stoppedWebhook <-chan struct{}

	// start webhook proxy
	if *webhookServePort > 0 {
		opts := &webhookproxy.Options{
			CertDir:     *webhookCertDir,
			BindPort:    *proxyWebhookPort,
			WebhookPort: *webhookServePort,
			ProxyClient: proxyClient,
		}
		proxy := webhookproxy.NewProxy(opts)
		readyHandler.Checks["webhookProxy"] = proxy.HealthFunc
		stoppedWebhook, err = proxy.Start(ctx)
		if err != nil {
			klog.Fatalf("Failed to start webhook proxy: %v", err)
		}
	} else {
		klog.Infof("Skip proxy webhook for webhook serve port not set")
	}

	// start apiserver proxy
	{
		opts := apiserverproxy.NewOptions()
		opts.Config = rest.CopyConfig(cfg)
		opts.SecureServingOptions.ServerCert.CertKey.KeyFile = "/var/run/secrets/kubernetes.io/serviceaccount/ctrlmesh/tls.key"
		opts.SecureServingOptions.ServerCert.CertKey.CertFile = "/var/run/secrets/kubernetes.io/serviceaccount/ctrlmesh/tls.crt"
		opts.SecureServingOptions.BindAddress = net.ParseIP("127.0.0.1")
		opts.SecureServingOptions.BindPort = *proxyApiserverPort
		opts.LeaderElectionName = *leaderElectionName
		opts.ProxyClient = proxyClient
		errs := opts.Validate()
		if len(errs) > 0 {
			klog.Fatalf("Failed to validate apiserver-proxy options %s: %v", util.DumpJSON(opts), errs)
		}
		proxy, err := apiserverproxy.NewProxy(opts)
		if err != nil {
			klog.Fatalf("Failed to new apiserver proxy: %v", err)
		}

		stoppedApiserver, err = proxy.Start(ctx)
		if err != nil {
			klog.Fatalf("Failed to start apiserver proxy: %v", err)
		}
	}

	serveHTTP(ctx, readyHandler)
	if stoppedWebhook != nil {
		select {
		case <-stoppedWebhook:
			klog.Infof("Webhook proxy stopped")
		}
	}
	select {
	case <-stoppedApiserver:
		klog.Infof("Apiserver proxy stopped")
	}
}

func serveHTTP(ctx context.Context, readyHandler *healthz.Handler) {
	mux := http.DefaultServeMux
	mux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.HTTPErrorOnError,
	}))
	mux.Handle("/readyz", http.StripPrefix("/readyz", readyHandler))

	server := http.Server{
		Handler: mux,
	}

	// Run the server
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *metricsHealthPort))
	if err != nil {
		klog.Fatalf("Failed to listen on :%d: %v", *metricsHealthPort, err)
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Failed to serve HTTP on :%d: %v", *metricsHealthPort, err)
		}
	}()

	<-ctx.Done()
	if err := server.Shutdown(context.Background()); err != nil {
		klog.Fatalf("Serve HTTP shutting down on :%d: %v", *metricsHealthPort, err)
	}
}

func getRestConfig() (*rest.Config, error) {
	const (
		tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		//rootCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/..data/ca.crt"
	)
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(host) == 0 || len(port) == 0 {
		return nil, rest.ErrNotInCluster
	}

	token, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}

	tlsClientConfig := rest.TLSClientConfig{Insecure: true}

	//if _, err := certutil.NewPool(rootCAFile); err != nil {
	//	klog.Errorf("Expected to load root CA config from %s, but got err: %v", rootCAFile, err)
	//} else {
	//	tlsClientConfig.CAFile = rootCAFile
	//}

	cfg := &rest.Config{
		// TODO: switch to using cluster DNS.
		Host:            "https://" + net.JoinHostPort(host, port),
		TLSClientConfig: tlsClientConfig,
		BearerToken:     string(token),
		BearerTokenFile: tokenFile,

		Burst: 3000,
		QPS:   2000.0,
	}
	klog.V(3).Infof("Starting with rest config: %v", util.DumpJSON(cfg))

	return cfg, nil
}
