// Package sockettest gives tests a short-lived directory for a unix domain
// socket. A socket path must fit inside sun_path (104 bytes on darwin, 108
// on linux), but t.TempDir() nests under the process's $TMPDIR, embedding
// the test's own name plus a per-subtest counter
// (/var/folders/<hash>/<hash>/T/<TestName><digits>/<NNN> on the macOS
// release runner); a long test name alone can push that past the limit
// before a socket filename is even appended. This bypasses $TMPDIR and
// asks for a directory directly under /tmp, so the result stays short
// regardless of the calling test's name.
package sockettest

import (
	"os"
	"testing"
)

// Dir returns a short directory suitable for holding a unix domain socket,
// removed via t.Cleanup.
func Dir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "plect-sock-")
	if err != nil {
		t.Fatalf("create short temp dir for unix socket: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
