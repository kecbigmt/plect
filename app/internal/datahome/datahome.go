// Package datahome resolves plect's runtime data directory (docs/design/sqlite-persistence.md).
package datahome

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvVar is plect's own data-home override, never passed to a child (see InheritableEnv).
const EnvVar = "PLECT_DATA_HOME"

// XDGEnvVar is the XDG Base Directory data variable, Resolve's fallback.
const XDGEnvVar = "XDG_DATA_HOME"

// Resolve returns EnvVar if set (no "/plect" suffix), else XDGEnvVar+"/plect", else ~/.local/share/plect.
func Resolve() string {
	if v := os.Getenv(EnvVar); v != "" {
		return v
	}
	dataHome := os.Getenv(XDGEnvVar)
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "plect")
}

// InheritableEnv returns os.Environ() minus EnvVar; XDGEnvVar stays, since other declarations still need it.
func InheritableEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if ok && key == EnvVar {
			continue
		}
		out = append(out, kv)
	}
	return out
}
