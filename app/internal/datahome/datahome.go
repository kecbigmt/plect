// Package datahome resolves plect's runtime data directory and the base
// environment a child process plect starts should inherit. See
// docs/design/sqlite-persistence.md, "Data-home resolution".
package datahome

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvVar is plect's own data-home override; unlike XDGEnvVar, a process
// never passes it on to a child it starts (see InheritableEnv).
const EnvVar = "PLECT_DATA_HOME"

// XDGEnvVar is the XDG Base Directory data variable this package honors as
// a fallback between EnvVar and the hardcoded default.
const XDGEnvVar = "XDG_DATA_HOME"

// Resolve returns the active plect data directory: EnvVar if set (used
// directly, with no "/plect" suffix), else XDGEnvVar+"/plect" if set, else
// ~/.local/share/plect.
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

// InheritableEnv returns os.Environ() with EnvVar removed, appended by a
// caller's own explicit binding for it (which then wins, per os/exec's
// last-value-for-a-duplicate-key rule). XDGEnvVar is left untouched: unlike
// EnvVar, existing declarations rely on a child inheriting it for their own
// unrelated on-disk state.
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
