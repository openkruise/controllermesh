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

package pod

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/openkruise/controllermesh/apis/ctrlmesh/constants"
	ctrlmeshv1alpha1 "github.com/openkruise/controllermesh/apis/ctrlmesh/v1alpha1"
	"github.com/openkruise/controllermesh/util"
	webhookutil "github.com/openkruise/controllermesh/webhook/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	initImage             = flag.String("init-image", "", "The image for ControllerMesh init container.")
	proxyImage            = flag.String("proxy-image", "", "The image for ControllerMesh proxy container.")
	proxyImagePullSecrets = flag.String("proxy-image-pull-secrets", "", "Image pull secrets in the namespace of ctrlmesh-manager if need.")

	proxyImagePullPolicy = flag.String("proxy-image-pull-policy", "Always", "Image pull policy for ControllerMesh proxy container, can be Always or IfNotPresent.")
	proxyResourceCPU     = flag.String("proxy-cpu", "100m", "The CPU limit for ControllerMesh proxy container.")
	proxyResourceMemory  = flag.String("proxy-memory", "200Mi", "The Memory limit for ControllerMesh proxy container.")
	proxyLogLevel        = flag.Uint("proxy-logv", 3, "The log level of ControllerMesh proxy container.")
	proxyExtraEnvs       = flag.String("proxy-extra-envs", "", "Extra environments for ControllerMesh proxy container.")
)

// +kubebuilder:rbac:groups=ctrlmesh.kruise.io,resources=virtualapps,verbs=get;list;watch

func (h *MutatingHandler) injectByVirtualApp(ctx context.Context, pod *v1.Pod) (retErr error) {
	virtualAppList := &ctrlmeshv1alpha1.VirtualAppList{}
	if err := h.Client.List(ctx, virtualAppList, client.InNamespace(pod.Namespace)); err != nil {
		return err
	}

	var matchedVApp *ctrlmeshv1alpha1.VirtualApp
	for i := range virtualAppList.Items {
		vApp := &virtualAppList.Items[i]
		selector, err := util.ValidatedLabelSelectorAsSelector(vApp.Spec.Selector)
		if err != nil {
			klog.Warningf("Failed to convert selector for VirtualApp %s/%s: %v", vApp.Namespace, vApp.Name, err)
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			if matchedVApp != nil {
				klog.Warningf("Find multiple VirtualApp %s %s matched Pod %s/%s", matchedVApp.Name, vApp.Name, pod.Namespace, pod.Name)
				return fmt.Errorf("multiple VirtualApp %s %s matched", matchedVApp.Name, vApp.Name)
			}
			matchedVApp = vApp
		}
	}
	if matchedVApp == nil {
		return nil
	}

	var initContainer *v1.Container
	var proxyContainer *v1.Container
	defer func() {
		if retErr == nil {
			klog.Infof("Successfully inject VirtualApp %s for Pod %s/%s creation, init: %s, sidecar: %s",
				matchedVApp.Name, pod.Namespace, pod.Name, util.DumpJSON(initContainer), util.DumpJSON(proxyContainer))
		} else {
			klog.Warningf("Failed to inject VirtualApp %s for Pod %s/%s creation, error: %v",
				matchedVApp.Name, pod.Namespace, pod.Name, retErr)
		}
	}()

	if pod.Spec.HostNetwork {
		return fmt.Errorf("can not use ControllerMesh for Pod with host network")
	}
	if *initImage == "" || *proxyImage == "" {
		return fmt.Errorf("the images for ControllerMesh init or proxy container have not set in args")
	}

	imagePullPolicy := v1.PullAlways
	if *proxyImagePullPolicy == string(v1.PullIfNotPresent) {
		imagePullPolicy = v1.PullIfNotPresent
	}

	initContainer = &v1.Container{
		Name:            constants.InitContainerName,
		Image:           *initImage,
		ImagePullPolicy: imagePullPolicy,
		SecurityContext: &v1.SecurityContext{
			Privileged:   utilpointer.BoolPtr(true),
			Capabilities: &v1.Capabilities{Add: []v1.Capability{"NET_ADMIN"}},
		},
		VolumeMounts: []v1.VolumeMount{
			{Name: constants.VolumeName, MountPath: constants.VolumeMountPath},
		},
	}
	proxyContainer = &v1.Container{
		Name:            constants.ProxyContainerName,
		Image:           *proxyImage,
		ImagePullPolicy: imagePullPolicy,
		Args: []string{
			"--v=" + strconv.Itoa(int(*proxyLogLevel)),
		},
		Env: []v1.EnvVar{
			{Name: constants.EnvPodName, ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: constants.EnvPodNamespace, ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: constants.EnvPodIP, ValueFrom: &v1.EnvVarSource{FieldRef: &v1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		},
		Lifecycle: &v1.Lifecycle{
			PostStart: &v1.Handler{
				Exec: &v1.ExecAction{Command: []string{"/bin/sh", "-c", "/poststart.sh"}},
			},
		},
		ReadinessProbe: &v1.Probe{
			Handler:       v1.Handler{HTTPGet: &v1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(constants.ProxyMetricsHealthPort)}},
			PeriodSeconds: 3,
		},
		Resources: v1.ResourceRequirements{
			Limits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse(*proxyResourceCPU),
				v1.ResourceMemory: resource.MustParse(*proxyResourceMemory),
			},
			Requests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("0"),
				v1.ResourceMemory: resource.MustParse("0"),
			},
		},
		SecurityContext: &v1.SecurityContext{
			Privileged:             utilpointer.BoolPtr(true), // This can be false, but true help us debug more easier.
			ReadOnlyRootFilesystem: utilpointer.BoolPtr(true),
			RunAsUser:              utilpointer.Int64Ptr(int64(constants.ProxyUserID)),
		},
		VolumeMounts: []v1.VolumeMount{
			{Name: constants.VolumeName, MountPath: constants.VolumeMountPath},
		},
	}

	if envs := getExtraEnvs(); len(envs) > 0 {
		proxyContainer.Env = append(proxyContainer.Env, envs...)
	}

	apiserverHostPortEnvs, err := getKubernetesServiceHostPort(pod)
	if err != nil {
		return err
	}
	if len(apiserverHostPortEnvs) > 0 {
		initContainer.Env = append(initContainer.Env, apiserverHostPortEnvs...)
		proxyContainer.Env = append(proxyContainer.Env, apiserverHostPortEnvs...)
	}

	if matchedVApp.Spec.Configuration.Controller != nil {
		proxyContainer.Args = append(
			proxyContainer.Args,
			fmt.Sprintf("--%s=%v", constants.ProxyLeaderElectionNameFlag, matchedVApp.Spec.Configuration.Controller.LeaderElectionName),
		)
	}

	if matchedVApp.Spec.Configuration.Webhook != nil {
		initContainer.Env = append(
			initContainer.Env,
			v1.EnvVar{Name: constants.EnvInboundWebhookPort, Value: strconv.Itoa(matchedVApp.Spec.Configuration.Webhook.Port)},
		)
		proxyContainer.Args = append(
			proxyContainer.Args,
			fmt.Sprintf("--%s=%v", constants.ProxyWebhookCertDirFlag, matchedVApp.Spec.Configuration.Webhook.CertDir),
			fmt.Sprintf("--%s=%v", constants.ProxyWebhookServePortFlag, matchedVApp.Spec.Configuration.Webhook.Port),
		)

		certVolumeMounts := getCertVolumeMounts(pod, matchedVApp.Spec.Configuration.Webhook.CertDir)
		if len(certVolumeMounts) > 1 {
			return fmt.Errorf("find multiple volume mounts that mount at %s: %v", matchedVApp.Spec.Configuration.Webhook.CertDir, certVolumeMounts)
		} else if len(certVolumeMounts) == 0 {
			return fmt.Errorf("find no volume mounts that mount at %s", matchedVApp.Spec.Configuration.Webhook.CertDir)
		}
		proxyContainer.VolumeMounts = append(proxyContainer.VolumeMounts, certVolumeMounts[0])
	}

	if matchedVApp.Spec.Configuration.RestConfigOverrides != nil {
		if matchedVApp.Spec.Configuration.RestConfigOverrides.UserAgentOrPrefix != nil {
			proxyContainer.Args = append(
				proxyContainer.Args,
				fmt.Sprintf("--%s=%v", constants.ProxyUserAgentOverrideFlag, *matchedVApp.Spec.Configuration.RestConfigOverrides.UserAgentOrPrefix),
			)
		}
	}

	pod.Spec.InitContainers = append([]v1.Container{*initContainer}, pod.Spec.InitContainers...)
	pod.Spec.Containers = append([]v1.Container{*proxyContainer}, pod.Spec.Containers...)
	pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{Name: constants.VolumeName, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}})
	if proxyImagePullSecrets != nil && len(*proxyImagePullSecrets) > 0 {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, h.getImagePullSecrets(pod.Namespace, pod.Name, strings.Split(*proxyImagePullSecrets, ","))...)
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[ctrlmeshv1alpha1.VirtualAppInjectedKey] = matchedVApp.Name
	return nil
}

func (h *MutatingHandler) getImagePullSecrets(podNamespace, podName string, secretNames []string) (refs []v1.LocalObjectReference) {
	managerNamespace := webhookutil.GetNamespace()
	if managerNamespace == podNamespace {
		for _, name := range secretNames {
			refs = append(refs, v1.LocalObjectReference{Name: name})
		}
		return
	}

	for _, secretName := range secretNames {
		secret := v1.Secret{}
		err := h.Client.Get(context.TODO(), types.NamespacedName{Namespace: podNamespace, Name: secretName}, &secret)
		if err == nil {
			refs = append(refs, v1.LocalObjectReference{Name: secretName})
			continue
		}

		injectionSecretName := fmt.Sprintf("ctrlmesh-injection-%s", secretName)
		err = h.Client.Get(context.TODO(), types.NamespacedName{Namespace: podNamespace, Name: injectionSecretName}, &secret)
		if err == nil {
			refs = append(refs, v1.LocalObjectReference{Name: injectionSecretName})
			continue
		}

		// create a new secret in the pod namespace
		err = h.Client.Get(context.TODO(), types.NamespacedName{Namespace: managerNamespace, Name: secretName}, &secret)
		if err != nil {
			klog.Warningf("Failed to inject imagePullSecret %s for Pod %s/%s, get the secret in %s error: %v",
				secretName, podNamespace, podName, managerNamespace, err)
			continue
		}
		newSecret := v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: podNamespace, Name: injectionSecretName},
			Data:       secret.Data,
		}
		err = h.Client.Create(context.TODO(), &newSecret)
		if err != nil {
			klog.Warningf("Failed to create imagePullSecret %s for Pod %s/%s: %v",
				util.DumpJSON(newSecret), podNamespace, podName, err)
			continue
		}
		refs = append(refs, v1.LocalObjectReference{Name: injectionSecretName})
	}
	return
}

func getKubernetesServiceHostPort(pod *v1.Pod) (vars []v1.EnvVar, err error) {
	var hostEnv *v1.EnvVar
	var portEnv *v1.EnvVar
	for i := range pod.Spec.Containers {
		if envVar := util.GetContainerEnvVar(&pod.Spec.Containers[i], "KUBERNETES_SERVICE_HOST"); envVar != nil {
			if hostEnv != nil && hostEnv.Value != envVar.Value {
				return nil, fmt.Errorf("found multiple KUBERNETES_SERVICE_HOST values: %v, %v", hostEnv.Value, envVar.Value)
			}
			hostEnv = envVar
		}
		if envVar := util.GetContainerEnvVar(&pod.Spec.Containers[i], "KUBERNETES_SERVICE_PORT"); envVar != nil {
			if portEnv != nil && portEnv.Value != envVar.Value {
				return nil, fmt.Errorf("found multiple KUBERNETES_SERVICE_PORT values: %v, %v", portEnv.Value, envVar.Value)
			}
			portEnv = envVar
		}
	}
	if hostEnv != nil {
		vars = append(vars, *hostEnv)
	}
	if portEnv != nil {
		vars = append(vars, *portEnv)
	}
	return vars, nil
}

func getCertVolumeMounts(pod *v1.Pod, cerDir string) (mounts []v1.VolumeMount) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]

		for _, m := range c.VolumeMounts {
			if m.MountPath == cerDir {
				// do not modify the ref
				m.ReadOnly = true
				mounts = append(mounts, m)
			}
		}
	}
	return
}

func getExtraEnvs() (envs []v1.EnvVar) {
	if len(*proxyExtraEnvs) == 0 {
		return
	}
	kvs := strings.Split(*proxyExtraEnvs, ";")
	for _, str := range kvs {
		kv := strings.Split(str, "=")
		if len(kv) != 2 {
			continue
		}
		envs = append(envs, v1.EnvVar{Name: kv[0], Value: kv[1]})
	}
	return
}
