package commands

import (
	"path/filepath"
	"testing"
)

func TestDefaultSessionMcpListenSocket(t *testing.T) {
	t.Run("under XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		got := defaultSessionMcpListenSocket("acme/widgets-758+claude")
		want := "/run/user/1000/plect-mcp/acme/widgets-758+claude.sock"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// /tmp, not os.TempDir(): on the darwin release runner, $TMPDIR is a long
	// per-process path (/var/folders/<hash>/<hash>/T), and a session name of
	// realistic length ("<org>/<repo>-<issue>+<workflow>") pushes the joined
	// socket path past the 104-byte sun_path limit, failing net.Listen with
	// "bind: invalid argument". /tmp keeps the fallback short regardless of
	// session name length, matching the convention the shipped claude
	// runtime effect's own socket path already uses
	// (${XDG_RUNTIME_DIR:-/tmp}/claude-channel).
	t.Run("falls back to /tmp without XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		got := defaultSessionMcpListenSocket("owner/session")
		want := filepath.Join("/tmp", "plect-mcp", "owner/session.sock")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
