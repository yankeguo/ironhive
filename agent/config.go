package agent

import (
	"fmt"
	"log"
	"os"
	pathpkg "path"

	"sigs.k8s.io/yaml"
)

// Default values for the agent configuration.
const (
	// DefaultConfigPath is where the agent looks for its config file; the
	// conventional location for an image to bake one in, under the
	// /opt/ironhive base the agent is implanted into (its binary is
	// injected at /opt/ironhive/bin via a shared volume; etc/ stays in
	// the image's own filesystem).
	DefaultConfigPath = "/opt/ironhive/etc/agent.yml"
	DefaultListen     = ":19173"
)

// Config is the agent's config file: an `http` section and the
// `allowed_envs` shell env passthrough list. It lets an image customize
// the injected agent's behavior — e.g. which of its own vars (APP_HOME,
// JAVA_OPTS) reach sandboxed commands — without an agent rebuild.
type Config struct {
	// HTTP configures the agent's own HTTP server.
	HTTP HTTPConfig `json:"http"`
	// AllowedEnvs is a full replacement for envAllowlist: wildcard
	// patterns (`*` / `?` / `[...]` as in path.Match) naming the process
	// environment variables shell commands may inherit when strict_env is
	// false. It is a pointer so a missing field (fall back to
	// envAllowlist) is distinguishable from an explicitly empty list
	// (pass nothing through).
	AllowedEnvs *[]string `json:"allowed_envs"`
}

// HTTPConfig is the `http` section.
type HTTPConfig struct {
	// Listen is the HTTP listen address.
	Listen string `json:"listen"`
}

// LoadConfig reads, parses and defaults the config file at path. A missing
// file is reported as errors.Is(err, fs.ErrNotExist) so the caller can
// decide whether that is fatal; a present-but-invalid file is an error —
// misconfiguration must not boot silently.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

// NewConfig returns a Config with all defaults applied, for running
// without a config file.
func NewConfig() *Config {
	cfg := &Config{}
	cfg.applyDefaults()
	return cfg
}

func (c *Config) applyDefaults() {
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = DefaultListen
	}
}

// envPatterns resolves allowed_envs into validated wildcard patterns;
// override reports whether the field was set at all and envAllowlist is
// replaced. Invalid patterns are dropped with a log so a typo surfaces
// instead of silently never matching.
func (c *Config) envPatterns() (patterns []string, override bool) {
	if c.AllowedEnvs == nil {
		return nil, false
	}
	for _, p := range *c.AllowedEnvs {
		if _, err := pathpkg.Match(p, ""); err != nil {
			log.Printf("agent: ignoring invalid allowed_envs pattern %q: %v", p, err)
			continue
		}
		patterns = append(patterns, p)
	}
	return patterns, true
}
