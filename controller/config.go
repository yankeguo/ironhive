package controller

import (
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// Default values for pool configuration entries.
const (
	DefaultStandbyStaticCount = 10
	DefaultAgentPort          = 19173
	DefaultListen             = ":8080"
)

// Config is the controller's config.yml: an `http` section, a
// `kubernetes` section, and the container `pools`.
type Config struct {
	// HTTP configures the controller's own HTTP server.
	HTTP HTTPConfig `json:"http"`
	// Kubernetes configures access to the cluster hosting the managed
	// pods.
	Kubernetes KubernetesConfig `json:"kubernetes"`
	// Pools maps pool names to their configuration.
	Pools map[string]PoolConfig `json:"pools"`
}

// HTTPConfig is the `http` section.
type HTTPConfig struct {
	// Listen is the HTTP listen address.
	Listen string `json:"listen"`
}

// KubernetesConfig is the `kubernetes` section.
type KubernetesConfig struct {
	// Kubeconfig is an explicit kubeconfig path; empty selects the
	// standard loading rules with the in-cluster config as fallback.
	Kubeconfig string `json:"kubeconfig"`
	// Namespace is where managed pods live; empty selects the in-cluster
	// service-account namespace, else "default".
	Namespace string `json:"namespace"`
}

// PoolConfig describes one pool of managed containers.
type PoolConfig struct {
	// Standby controls how many pods are kept warm, ready to be assigned.
	Standby StandbyConfig `json:"standby"`
	// PodTemplate is the Kubernetes pod template pods of this pool are
	// created from, metadata and spec in the usual shape. Labels and
	// annotations set here are merged with the controller's enforced
	// management entries when a pod is created; controller-owned
	// allocation annotations are stripped.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
	// Agent describes the ironhive-agent endpoint inside each pod.
	Agent PoolAgentConfig `json:"agent"`
}

// StandbyConfig groups the standby sizing strategies; only static exists
// for now.
type StandbyConfig struct {
	// Static keeps a fixed number of warm pods.
	Static StaticStandbyConfig `json:"static"`
}

// StaticStandbyConfig is the fixed-size standby strategy.
type StaticStandbyConfig struct {
	// Count is the number of warm pods to keep; 0 selects the default.
	Count int `json:"count"`
}

// PoolAgentConfig describes the agent endpoint inside a pool's pods.
type PoolAgentConfig struct {
	// Port the agent listens on; 0 selects the default.
	Port int `json:"port"`
}

// LoadConfig reads, parses, defaults and validates the config file at
// path. A missing file is reported as errors.Is(err, fs.ErrNotExist) so
// the caller can decide whether that is fatal.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// sigs.k8s.io/yaml goes through JSON, so the embedded PodTemplateSpec
	// parses with its native (camelCase) field names and strictness.
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// NewConfig returns a Config with all defaults applied and no pools, for
// running without a config file.
func NewConfig() *Config {
	cfg := &Config{}
	cfg.applyDefaults()
	return cfg
}

func (c *Config) applyDefaults() {
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = DefaultListen
	}
	if c.Kubernetes.Namespace == "" {
		c.Kubernetes.Namespace = DefaultNamespace()
	}
	for name, p := range c.Pools {
		if p.Standby.Static.Count == 0 {
			p.Standby.Static.Count = DefaultStandbyStaticCount
		}
		if p.Agent.Port == 0 {
			p.Agent.Port = DefaultAgentPort
		}
		c.Pools[name] = p
	}
}

func (c *Config) validate() error {
	if errs := validation.IsDNS1123Label(c.Kubernetes.Namespace); len(errs) > 0 {
		return fmt.Errorf("kubernetes.namespace %q: %s", c.Kubernetes.Namespace, strings.Join(errs, "; "))
	}
	for name, p := range c.Pools {
		// The pool name lands in the ironhive.dev/pool label of every pod,
		// so it must be a valid label value — otherwise pod creation fails
		// at runtime with a far less obvious error.
		if errs := validation.IsValidLabelValue(name); len(errs) > 0 {
			return fmt.Errorf("pool %q: invalid name: %s", name, strings.Join(errs, "; "))
		}
		if p.Standby.Static.Count < 0 {
			return fmt.Errorf("pool %q: standby.static.count must be >= 0", name)
		}
		if p.Agent.Port < 1 || p.Agent.Port > 65535 {
			return fmt.Errorf("pool %q: agent.port %d out of range", name, p.Agent.Port)
		}
	}
	return nil
}
