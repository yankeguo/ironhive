package controller

import (
	"errors"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewKubernetesClient builds a clientset, resolving credentials in order:
//
//  1. an explicit kubeconfig path (flag / IHC_KUBECONFIG),
//  2. the default loading rules ($KUBECONFIG, then ~/.kube/config),
//  3. the in-cluster service-account config — the fallback for running
//     inside a pod, which needs no configuration at all.
//
// A malformed kubeconfig fails hard instead of silently falling back, so
// misconfiguration is not masked. The returned string describes the
// credential source, for logging.
func NewKubernetesClient(kubeconfig string) (kubernetes.Interface, string, error) {
	config, source, err := kubeRESTConfig(kubeconfig)
	if err != nil {
		return nil, "", err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, "", fmt.Errorf("kubernetes client (%s): %w", source, err)
	}
	return client, source, nil
}

func kubeRESTConfig(kubeconfig string) (*rest.Config, string, error) {
	if kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, "", fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
		}
		return config, "kubeconfig " + kubeconfig, nil
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	switch {
	case err == nil:
		return config, "default kubeconfig", nil
	case errors.Is(err, clientcmd.ErrEmptyConfig):
		// No kubeconfig anywhere: assume we run inside a pod and fall
		// back to the mounted service account.
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, "", fmt.Errorf("no kubeconfig found and in-cluster config unavailable: %w", err)
		}
		return config, "in-cluster", nil
	default:
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
}
