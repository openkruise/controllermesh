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

package constants

const (
	ProxyUserID = 1359

	ProxyMetricsHealthPort = 5441
	ProxyApiserverPort     = 5443
	ProxyWebhookPort       = 5445
	ProxyGRPCPort          = 5447

	ProxyMetricsHealthPortFlag = "metrics-health-port"
	ProxyApiserverPortFlag     = "proxy-apiserver-port"
	ProxyWebhookPortFlag       = "proxy-webhook-port"
	ProxyGRPCPortFlag          = "grpc-port"

	ProxyLeaderElectionNameFlag = "leader-election-name"
	ProxyWebhookServePortFlag   = "webhook-serve-port"
	ProxyWebhookCertDirFlag     = "webhook-cert-dir"
	ProxyUserAgentOverrideFlag  = "user-agent-override"

	InitContainerName  = "ctrlmesh-init"
	ProxyContainerName = "ctrlmesh-proxy"

	EnvInboundWebhookPort = "INBOUND_WEBHOOK_PORT"
	EnvPodName            = "POD_NAME"
	EnvPodNamespace       = "POD_NAMESPACE"
	EnvPodIP              = "POD_IP"

	EnvDisableIptables = "CTRLMESH_DISABLE_IPTABLE"

	VolumeName      = "ctrlmesh"
	VolumeMountPath = "/ctrlmesh"
)
