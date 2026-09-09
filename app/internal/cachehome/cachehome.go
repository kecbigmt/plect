// Package cachehome resolves plect's plugin catalog cache root, kept independent of app/internal/datahome so a rehearsal can isolate the two separately.
package cachehome

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// EnvVar is plect's own cache-home override, never passed to a child (see Strip).
const EnvVar = "PLECT_CACHE_HOME"

// XDGEnvVar is the XDG Base Directory cache variable, Resolve's fallback.
const XDGEnvVar = "XDG_CACHE_HOME"

// Resolve returns EnvVar if set (no "/plect/catalogs" suffix), else XDGEnvVar+"/plect/catalogs", else ~/.cache/plect/catalogs.
// A home-directory lookup failure returns an error rather than a relative ".cache" a caller could mistake for one rooted at the working directory.
func Resolve() (string, error) {
	if v := os.Getenv(EnvVar); v != "" {
		return v, nil
	}
	cacheHome := os.Getenv(XDGEnvVar)
	if cacheHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		cacheHome = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheHome, "plect", "catalogs"), nil
}

// Strip removes EnvVar from env but keeps XDGEnvVar, since a declaration-started child may still depend on inheriting the shared XDG variable for its own state.
func Strip(env []string) []string {
	return without(env, EnvVar)
}

// StripIsolated removes both EnvVar and XDGEnvVar, for a child whose destination is untrusted enough that even the shared XDG variable must not leak.
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
