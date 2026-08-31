package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigMissing(t *testing.T) {
	// A missing file is fs.ErrNotExist, so the caller can fall back to
	// defaults without special-casing.
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadConfig(missing) err = %v, want fs.ErrNotExist", err)
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	// A present-but-unparsable file is a hard failure, not a silent
	// fallback.
	if _, err := LoadConfig(writeConfig(t, "allowed_envs: [unclosed\n")); err == nil {
		t.Fatal("LoadConfig(invalid) err = nil, want error")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	// Empty file and partial sections both get the defaults applied.
	for _, content := range []string{"", "allowed_envs: [PATH]\n"} {
		cfg, err := LoadConfig(writeConfig(t, content))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.HTTP.Listen != DefaultListen {
			t.Fatalf("Listen = %q, want %q", cfg.HTTP.Listen, DefaultListen)
		}
	}
	cfg, err := LoadConfig(writeConfig(t, "http:\n  listen: ':12345'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != ":12345" {
		t.Fatalf("Listen = %q, want :12345", cfg.HTTP.Listen)
	}
}

func TestNewConfig(t *testing.T) {
	if cfg := NewConfig(); cfg.HTTP.Listen != DefaultListen {
		t.Fatalf("Listen = %q, want %q", cfg.HTTP.Listen, DefaultListen)
	}
}

func TestConfigEnvPatterns(t *testing.T) {
	// A missing field means no override: the built-in allowlist applies.
	if _, override := NewConfig().envPatterns(); override {
		t.Fatal("envPatterns(no field) override = true, want false")
	}
	cfg, err := LoadConfig(writeConfig(t, "other_field: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, override := cfg.envPatterns(); override {
		t.Fatal("envPatterns(no field) override = true, want false")
	}

	// An explicitly empty list is an override too: nothing passes through.
	cfg, err = LoadConfig(writeConfig(t, "allowed_envs: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	patterns, override := cfg.envPatterns()
	if !override || len(patterns) != 0 {
		t.Fatalf("envPatterns(empty) = %v, %v, want [], true", patterns, override)
	}

	// Comments, quoted entries and invalid patterns (ignored with a log).
	cfg, err = LoadConfig(writeConfig(t,
		"# image-specific vars\nallowed_envs:\n  - APP_*\n  - 'JAVA_OPTS'\n  - '[BAD'\n  - '?'\n"))
	if err != nil {
		t.Fatal(err)
	}
	patterns, override = cfg.envPatterns()
	want := []string{"APP_*", "JAVA_OPTS", "?"}
	if !override || len(patterns) != len(want) {
		t.Fatalf("envPatterns = %v, %v, want %v, true", patterns, override, want)
	}
	for i := range want {
		if patterns[i] != want[i] {
			t.Fatalf("envPatterns = %v, want %v", patterns, want)
		}
	}
}
