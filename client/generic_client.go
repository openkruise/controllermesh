package client

import (
	ctrlmeshclientset "github.com/openkruise/controllermesh/client/clientset/versioned"
	"k8s.io/client-go/discovery"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// GenericClientset defines a generic client
type GenericClientset struct {
	DiscoveryClient discovery.DiscoveryInterface
	KubeClient      kubeclientset.Interface
	CtrlmeshClient  ctrlmeshclientset.Interface
}

// newForConfig creates a new Clientset for the given config.
func newForConfig(c *rest.Config) (*GenericClientset, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(c)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubeclientset.NewForConfig(c)
	if err != nil {
		return nil, err
	}
	ctrlmeshClient, err := ctrlmeshclientset.NewForConfig(c)
	if err != nil {
		return nil, err
	}
	return &GenericClientset{
		DiscoveryClient: discoveryClient,
		KubeClient:      kubeClient,
		CtrlmeshClient:  ctrlmeshClient,
	}, nil
}

// newForConfig creates a new Clientset for the given config.
func newForConfigOrDie(c *rest.Config) *GenericClientset {
	gc, err := newForConfig(c)
	if err != nil {
		panic(err)
	}
	return gc
}
