package commands

import (
	"path/filepath"
	"testing"
)

func TestDefaultSessionMcpListenSocket(t *testing.T) {
	t.Run("under XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		got, needsPrivateRoot := defaultSessionMcpListenSocket("acme/widgets-758+claude", "/should/not/be/used")
		want := "/run/user/1000/plect-mcp/acme/widgets-758+claude.sock"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if needsPrivateRoot {
			t.Error("needsPrivateRoot = true, want false under $XDG_RUNTIME_DIR")
		}
	})

	t.Run("falls back to the given root without XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		got, needsPrivateRoot := defaultSessionMcpListenSocket("owner/session", "/tmp/plect-mcp-test")
		want := filepath.Join("/tmp/plect-mcp-test", "owner/session.sock")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if !needsPrivateRoot {
			t.Error("needsPrivateRoot = false, want true for the fallback root")
		}
	})
}
