// Package config reads Blackbox's settings from the environment. Secrets arrive
// under their Cove key names; everything else is fixed in code.
package config

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// Config is what every part of Blackbox needs.
type Config struct {
	Env     string // "prod" or "dev"
	Version string // BLACKBOX_VERSION, or "dev-<commit>" / "dev" for a local build
}

// Load reads the settings through getenv (os.Getenv outside tests).
func Load(getenv func(string) string) (Config, error) {
	env := strings.ToLower(strings.TrimSpace(getenv("APP_ENV")))
	switch env {
	case "":
		env = "prod"
	case "prod", "dev":
	default:
		return Config{}, fmt.Errorf("config: APP_ENV=%q isn't valid: set it to prod or dev, or leave it out for prod", env)
	}

	version := strings.TrimSpace(getenv("BLACKBOX_VERSION"))
	if version == "" {
		version = buildVersion()
	}
	return Config{Env: env, Version: version}, nil
}

// Secret returns the environment variable name, which carries the Cove key of
// the same name; it's an error when it's missing.
func Secret(getenv func(string) string, name string) (string, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return "", fmt.Errorf("config: %s isn't set: in prod Lighthouse fills it from Cove, so the compose file must pass it through; locally it comes from .env", name)
	}
	return v, nil
}

// buildVersion is "dev-<commit>" when the binary was built from a git
// checkout, otherwise "dev".
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return "dev"
}
