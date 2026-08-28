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
	pool, ok := cfg.Pools["default"]
	if !ok {
		t.Fatalf("pools = %v", cfg.Pools)
	}
	if pool.Standby.Static.Count != DefaultStandbyStaticCount {
		t.Fatalf("standby.static.count = %d, want %d", pool.Standby.Static.Count, DefaultStandbyStaticCount)
	}
	if pool.Agent.Port != DefaultAgentPort {
		t.Fatalf("agent.port = %d, want %d", pool.Agent.Port, DefaultAgentPort)
	}
	if got := pool.PodTemplate.Spec.Containers[0].Image; got != "ghcr.io/yankeguo/ironhive:agent-latest" {
		t.Fatalf("podTemplate image = %q", got)
	}
}

func TestLoadConfigExplicitValues(t *testing.T) {
	p := writeConfig(t, `
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
    agent:
      port: 9090
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	pool := cfg.Pools["heavy"]
	if pool.Standby.Static.Count != 3 {
		t.Fatalf("count = %d, want 3", pool.Standby.Static.Count)
	}
	if pool.Agent.Port != 9090 {
		t.Fatalf("port = %d, want 9090", pool.Agent.Port)
	}
	if pool.PodTemplate.Labels["pool"] != "heavy" {
		t.Fatalf("labels = %v", pool.PodTemplate.Labels)
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
	// Out-of-range agent port.
	p = writeConfig(t, "pools:\n  x:\n    agent:\n      port: 70000\n")
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("bad port: err = %v", err)
	}
}
