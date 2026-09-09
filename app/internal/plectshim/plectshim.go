// Package plectshim builds the PATH shim an isolated child resolves a bare
// `plect` through to the daemon's own binary and store, by identity: a path
// with a slash (`./plect`, `go run`) bypasses PATH lookup and keeps
// resolving the isolated default (docs/design/sqlite-persistence.md).
package plectshim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/cachehome"
	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/datahome"
)

const shimName = "plect"

// EnvVar names the plect CLI executable explicitly for a non-CLI host with
// no "plect" shipped alongside it: resolveBin never guesses one from PATH,
// which could silently pick an unrelated, differently-versioned build.
const EnvVar = "PLECT_BIN"

// executablePath and forcedUnderTest are swappable by a test (testsupport.go).
var (
	executablePath  = os.Executable
	forcedUnderTest bool
)

// ForCurrentProcess builds the running process's shim and returns its dir.
// A caller treats a returned error as best-effort and skips the shim,
// rather than failing outright.
func ForCurrentProcess() (string, error) {
	// testing.Testing(), not a binary-name check (plect-web is as
	// legitimate a host as plect): shimming to a `go test` binary would
	// make a real script's bare `plect` call recurse into testing.Main.
	if testing.Testing() && !forcedUnderTest {
		return "", fmt.Errorf("plectshim: refusing to shim to a go test binary")
	}
	self, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("plectshim: resolve running executable: %w", err)
	}
	bin, err := resolveBin(self)
	if err != nil {
		return "", err
	}
	dataHome, err := filepath.Abs(datahome.Resolve())
	if err != nil {
		return "", fmt.Errorf("plectshim: resolve data home: %w", err)
	}
	configHome, err := overrideAbs(confighome.EnvVar, confighome.XDGEnvVar, confighome.Resolve)
	if err != nil {
		return "", err
	}
	cacheHome, err := overrideAbs(cachehome.EnvVar, cachehome.XDGEnvVar, cachehome.Resolve)
	if err != nil {
		return "", err
	}
	return Build(bin, dataHome, configHome, cacheHome)
}

// resolveBin returns self when named "plect", else its shipped sibling,
// else EnvVar: plect-web (webui.LiveService.Up) has no CLI subcommand tree.
func resolveBin(self string) (string, error) {
	if filepath.Base(self) == shimName {
		return self, nil
	}
	if sibling := filepath.Join(filepath.Dir(self), shimName); isExecutableFile(sibling) {
		return sibling, nil
	}
	if override := os.Getenv(EnvVar); override != "" {
		if !isExecutableFile(override) {
			return "", fmt.Errorf("plectshim: %s=%q is not an executable file", EnvVar, override)
		}
		abs, err := filepath.Abs(override) // the shim runs from a different cwd later
		if err != nil {
			return "", fmt.Errorf("plectshim: resolve %s: %w", EnvVar, err)
		}
		return abs, nil
	}
	return "", fmt.Errorf("plectshim: no plect executable found alongside %q; set %s", self, EnvVar)
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// overrideAbs resolves envVar/xdgEnvVar's value only when this process
// itself carries one; an isolated child keeps neither.
func overrideAbs(envVar, xdgEnvVar string, resolve func() (string, error)) (string, error) {
	if os.Getenv(envVar) == "" && os.Getenv(xdgEnvVar) == "" {
		return "", nil
	}
	v, err := resolve()
	if err != nil {
		return "", fmt.Errorf("plectshim: resolve %s override: %w", envVar, err)
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("plectshim: resolve %s override: %w", envVar, err)
	}
	return abs, nil
}

// Build writes the shim script under dataHome/bin, rewriting it on every
// call so it tracks a daemon rebuild or restart, and returns that
// directory. configHome/cacheHome are pinned only when non-empty.
func Build(bin, dataHome, configHome, cacheHome string) (string, error) {
	if bin == "" {
		return "", fmt.Errorf("plectshim: daemon executable path is empty")
	}
	if dataHome == "" {
		return "", fmt.Errorf("plectshim: data home is empty")
	}
	dir := filepath.Join(dataHome, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("plectshim: create shim directory: %w", err)
	}
	script := scriptFor(bin, dataHome, configHome, cacheHome)

	// Same-dir temp file + rename: atomic against a concurrent exec.
	tmp, err := os.CreateTemp(dir, ".plect-*")
	if err != nil {
		return "", fmt.Errorf("plectshim: create shim script: %w", err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.WriteString(script)
	closeErr := tmp.Close()
	if writeErr != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("plectshim: write shim script: %w", writeErr)
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("plectshim: write shim script: %w", closeErr)
	}
	if err := os.Chmod(tmpPath, 0o700); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("plectshim: make shim script executable: %w", err)
	}
	finalPath := filepath.Join(dir, shimName)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("plectshim: install shim script: %w", err)
	}
	return dir, nil
}

func scriptFor(bin, dataHome, configHome, cacheHome string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("exec " + shQuote(bin) + " --data-home " + shQuote(dataHome))
	if configHome != "" {
		b.WriteString(" --config-home " + shQuote(configHome))
	}
	if cacheHome != "" {
		b.WriteString(" --cache-home " + shQuote(cacheHome))
	}
	b.WriteString(` "$@"` + "\n")
	return b.String()
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PatchPath appends a new PATH entry (shimDir plus env's current PATH), so
// it wins under exec.Cmd's last-duplicate-key rule yet still yields to an
// explicit PATH appended after this call.
func PatchPath(env []string, shimDir string) []string {
	current := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			current = v
		}
	}
	newPath := shimDir
	if current != "" {
		newPath = shimDir + string(os.PathListSeparator) + current
	}
	return append(env, "PATH="+newPath)
}
