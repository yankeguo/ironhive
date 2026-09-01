package controller

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// serviceAccountNamespacePath is where Kubernetes mounts the pod's own
// namespace; a variable so tests can point it elsewhere.
var serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// DefaultNamespace resolves the namespace managed pods live in: the
// in-cluster service-account namespace when running inside a pod, else
// "default".
func DefaultNamespace() string {
	if data, err := os.ReadFile(serviceAccountNamespacePath); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return "default"
}

// NewKubernetesClient builds a clientset, resolving credentials in order:
//
//  1. an explicit kubeconfig path (the config file's `kubernetes.kubeconfig`),
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
	// client-go defaults to 5 QPS / 10 burst — a client-side rate limiter
	// that would throttle allocate/renew/release patches and reconcile
	// sweeps under any real churn. Raise it well past plausible load: the
	// API server enforces its own admission limits, so this process must
	// not bottleneck itself first.
	config.QPS = 200
	config.Burst = 400
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
	case clientcmd.IsEmptyConfig(err):
		// No kubeconfig anywhere: assume we run inside a pod and fall
		// back to the mounted service account. IsEmptyConfig is the
		// sanctioned check — errors.Is never matches here because the
		// loader's "no configuration" error is a slice without Unwrap.
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, "", fmt.Errorf("no kubeconfig found and in-cluster config unavailable: %w", err)
		}
		return config, "in-cluster", nil
	default:
		return nil, "", fmt.Errorf("kubeconfig: %w", err)
	}
}
