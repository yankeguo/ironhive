package controller

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigDefaults(t *testing.T) {
	p := writeConfig(t, `
pools:
  default:
    podTemplate:
      spec:
        containers:
          - name: agent
            image: ghcr.io/yankeguo/ironhive:agent-latest
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != DefaultListen {
		t.Fatalf("http.listen = %q, want %q", cfg.HTTP.Listen, DefaultListen)
	}
	if cfg.Kubernetes.Namespace != DefaultNamespace() {
		t.Fatalf("kubernetes.namespace = %q, want %q", cfg.Kubernetes.Namespace, DefaultNamespace())
	}
	pool, ok := cfg.Pools["default"]
	if !ok {
		t.Fatalf("pools = %v", cfg.Pools)
	}
	if pool.Standby.Static.Count != DefaultStandbyStaticCount {
		t.Fatalf("standby.static.count = %d, want %d", pool.Standby.Static.Count, DefaultStandbyStaticCount)
	}
	if got := pool.AgentPort(); got != DefaultAgentPort {
		t.Fatalf("AgentPort() = %d, want %d", got, DefaultAgentPort)
	}
	if got := pool.PodTemplate.Spec.Containers[0].Image; got != "ghcr.io/yankeguo/ironhive:agent-latest" {
		t.Fatalf("podTemplate image = %q", got)
	}
}

func TestLoadConfigExplicitValues(t *testing.T) {
	p := writeConfig(t, `
http:
  listen: ":9091"
kubernetes:
  kubeconfig: /etc/kube/admin.conf
  namespace: sandboxes
pools:
  heavy:
    standby:
      static:
        count: 3
    podTemplate:
      metadata:
        labels:
          pool: heavy
      spec:
        containers:
          - name: agent
            image: agent:dev
            ports:
              - containerPort: 8080
              - name: http-ironhive
                containerPort: 9090
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != ":9091" {
		t.Fatalf("http.listen = %q", cfg.HTTP.Listen)
	}
	if cfg.Kubernetes.Kubeconfig != "/etc/kube/admin.conf" {
		t.Fatalf("kubernetes.kubeconfig = %q", cfg.Kubernetes.Kubeconfig)
	}
	if cfg.Kubernetes.Namespace != "sandboxes" {
		t.Fatalf("kubernetes.namespace = %q", cfg.Kubernetes.Namespace)
	}
	pool := cfg.Pools["heavy"]
	if pool.Standby.Static.Count != 3 {
		t.Fatalf("count = %d, want 3", pool.Standby.Static.Count)
	}
	// The http-ironhive port wins over the first declared one.
	if got := pool.AgentPort(); got != 9090 {
		t.Fatalf("AgentPort() = %d, want 9090", got)
	}
	if pool.PodTemplate.Labels["pool"] != "heavy" {
		t.Fatalf("labels = %v", pool.PodTemplate.Labels)
	}
}

func TestPoolAgentPort(t *testing.T) {
	// No named port: the first declared container port wins.
	p := writeConfig(t, `
pools:
  x:
    podTemplate:
      spec:
        containers:
          - name: agent
            image: agent:dev
            ports:
              - containerPort: 8080
              - containerPort: 9090
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pools["x"].AgentPort(); got != 8080 {
		t.Fatalf("AgentPort() = %d, want 8080", got)
	}

	// A named http-ironhive port without containerPort parses as 0 and
	// must not win — the first declared port applies instead.
	p = writeConfig(t, `
pools:
  x:
    podTemplate:
      spec:
        containers:
          - name: agent
            image: agent:dev
            ports:
              - name: http-ironhive
              - containerPort: 8080
`)
	cfg, err = LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pools["x"].AgentPort(); got != 8080 {
		t.Fatalf("AgentPort() with zero named port = %d, want 8080", got)
	}

	// With no other declared port, the default applies.
	p = writeConfig(t, `
pools:
  x:
    podTemplate:
      spec:
        containers:
          - name: agent
            image: agent:dev
            ports:
              - name: http-ironhive
`)
	cfg, err = LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pools["x"].AgentPort(); got != DefaultAgentPort {
		t.Fatalf("AgentPort() with only a zero named port = %d, want %d", got, DefaultAgentPort)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	// A missing file is fs.ErrNotExist, for the caller to classify.
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want fs.ErrNotExist", err)
	}
	// Malformed YAML.
	if _, err := LoadConfig(writeConfig(t, "pools: [unclosed")); err == nil {
		t.Fatal("malformed yaml: want error")
	}
	// Negative standby count.
	p := writeConfig(t, "pools:\n  x:\n    standby:\n      static:\n        count: -1\n")
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("negative count: err = %v", err)
	}
	// A pool name that is not a valid label value.
	p = writeConfig(t, "pools:\n  'bad name': {}\n")
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("bad pool name: err = %v", err)
	}
	// Namespace names must be valid before any Kubernetes request is made.
	p = writeConfig(t, "kubernetes:\n  namespace: 'bad namespace'\n")
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "kubernetes.namespace") {
		t.Fatalf("bad namespace: err = %v", err)
	}
}
