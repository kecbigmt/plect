package commands

import (
	"fmt"
	"os"
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

	// A short, uid-scoped /tmp root, not os.TempDir(): on the darwin release
	// runner, $TMPDIR is a long per-process path (/var/folders/<hash>/<hash>/T),
	// and a session name of realistic length ("<org>/<repo>-<issue>+<workflow>")
	// pushes the joined socket path past the 104-byte sun_path limit, failing
	// net.Listen with "bind: invalid argument". uid-scoping (rather than a
	// single shared "/tmp/plect-mcp") also keeps the path from being a
	// predictable location another local user could pre-create.
	t.Run("falls back to a private per-uid /tmp root without XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		got := defaultSessionMcpListenSocket("owner/session")
		want := filepath.Join(fmt.Sprintf("/tmp/plect-mcp-%d", os.Getuid()), "owner/session.sock")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
