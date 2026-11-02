package config

import (
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "prod" {
		t.Errorf("Env = %q, want prod", cfg.Env)
	}
	if !strings.HasPrefix(cfg.Version, "dev") {
		t.Errorf("Version = %q, want dev or dev-<commit>", cfg.Version)
	}
}

func TestLoadValues(t *testing.T) {
	cfg, err := Load(env(map[string]string{"APP_ENV": " Dev ", "BLACKBOX_VERSION": "v0.1.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != "dev" || cfg.Version != "v0.1.0" {
		t.Errorf("got %+v, want dev and v0.1.0", cfg)
	}
}

func TestSecret(t *testing.T) {
	get := env(map[string]string{"BLACKBOX_DATABASE_URL": " postgres://x "})
	if v, err := Secret(get, "BLACKBOX_DATABASE_URL"); err != nil || v != "postgres://x" {
		t.Errorf("Secret = %q, %v", v, err)
	}
	if _, err := Secret(get, "BLACKBOX_DATABASE_MIGRATOR_URL"); err == nil || !strings.Contains(err.Error(), "Cove") {
		t.Errorf("missing: err = %v, want a hint about Cove", err)
	}
}

func TestLoadRefusesUnknownEnv(t *testing.T) {
	_, err := Load(env(map[string]string{"APP_ENV": "staging"}))
	if err == nil || !strings.Contains(err.Error(), "APP_ENV") {
		t.Fatalf("err = %v, want one naming APP_ENV", err)
	}
}
