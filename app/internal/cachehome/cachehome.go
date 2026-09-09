// Package cachehome resolves plect's plugin catalog cache root (mirrors app/internal/datahome).
package cachehome

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// EnvVar is plect's own cache-home override, never passed to a child.
const EnvVar = "PLECT_CACHE_HOME"

// XDGEnvVar is the XDG Base Directory cache variable, Resolve's fallback.
const XDGEnvVar = "XDG_CACHE_HOME"

// Resolve returns EnvVar if set (no "/plect/catalogs" suffix), else XDGEnvVar+"/plect/catalogs", else ~/.cache/plect/catalogs.
func Resolve() string {
	if v := os.Getenv(EnvVar); v != "" {
		return v
	}
	cacheHome := os.Getenv(XDGEnvVar)
	if cacheHome == "" {
		home, _ := os.UserHomeDir()
		cacheHome = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheHome, "plect", "catalogs")
}

// Strip removes EnvVar from env; compose with datahome.InheritableEnv.
func Strip(env []string) []string {
	return without(env, EnvVar)
}

// StripIsolated removes both EnvVar and XDGEnvVar; compose with datahome.IsolatedEnv.
func StripIsolated(env []string) []string {
	return without(env, EnvVar, XDGEnvVar)
}

func without(env []string, drop ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && slices.Contains(drop, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
