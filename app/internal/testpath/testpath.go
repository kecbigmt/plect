// Package testpath resolves symlinks in a test's expected path so it
// agrees with production code that does the same. On macOS, t.TempDir()
// returns a path under /var, which is itself a symlink to /private/var;
// production path-resolution code that calls filepath.EvalSymlinks (to
// look up sibling files next to a config's real location, or to check
// path containment) returns the /private/var form, so a test comparing
// against a raw t.TempDir() value fails only on that platform.
package testpath

import (
	"path/filepath"
	"testing"
)

// Real resolves symlinks in dir, matching what production code that calls
// filepath.EvalSymlinks on a path derived from dir would return.
func Real(t testing.TB, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve symlinks in %s: %v", dir, err)
	}
	return real
}
