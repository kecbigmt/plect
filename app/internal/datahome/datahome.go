// Package datahome resolves the directory plect's own runtime persistence
// (the SQLite store, the durable event log) lives in, and the base
// environment a child process plect starts should inherit.
package datahome

import (
	"os"
	"path/filepath"
	"strings"
)

// EnvVar is plect's own data-home override. Unlike XDGEnvVar, a process
// that sets it in its own environment does not pass it on to a child it
// starts (see InheritableEnv): a relocation of one plect process's own
// storage must not also relocate storage for an unrelated build started by
// something that process spawns (a tmux pane, a task's setup script, a
// plugin service, a channel command).
const EnvVar = "PLECT_DATA_HOME"

// XDGEnvVar is the XDG Base Directory data variable this package honors as
// a fallback between EnvVar and the hardcoded default.
const XDGEnvVar = "XDG_DATA_HOME"

// Resolve returns the active plect data directory: EnvVar if set — used
// directly, since the variable names the plect data directory itself and
// has no reason to carry XDGEnvVar's "/plect" namespacing inside a shared
// directory — else XDGEnvVar+"/plect" if set, else ~/.local/share/plect.
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

// InheritableEnv returns os.Environ() with EnvVar removed — the base
// environment for a child process a declaration (a task's setup/cleanup, a
// channel delivery, a plugin service, a resource observer's query) starts,
// so a plect-specific data-home relocation active in this process is not
// silently inherited by a build the child itself invokes. A caller with its
// own explicit binding for EnvVar appends it after this base, which wins per
// os/exec's last-value-for-a-duplicate-key rule.
//
// XDGEnvVar is deliberately left untouched here: unlike EnvVar, it is a
// general-purpose variable existing declarations already rely on a child
// inheriting for their own unrelated data (a resource observer or plugin
// service locating its own on-disk state, independent of plect's own
// store). Stripping it too would need those declarations to gain an
// explicit rebinding mechanism first; today only EnvVar's inheritance is
// plect-specific enough to strip unconditionally.
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
