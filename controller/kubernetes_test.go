package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewKubernetesClientExplicitKubeconfig(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	content := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://192.0.2.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: test-token
`
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	client, source, err := NewKubernetesClient(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}
	if source != "kubeconfig "+kubeconfig {
		t.Fatalf("source = %q", source)
	}
}

func TestNewKubernetesClientBadKubeconfig(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte("not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A malformed explicit kubeconfig fails hard — no silent fallback.
	if _, _, err := NewKubernetesClient(kubeconfig); err == nil {
		t.Fatal("want error for malformed kubeconfig")
	} else if !strings.Contains(err.Error(), kubeconfig) {
		t.Fatalf("error %q should name the kubeconfig path", err)
	}
}
