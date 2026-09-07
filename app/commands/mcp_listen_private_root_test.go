package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePrivateFallbackRoot_CreatesFreshDirWithPrivateMode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plect-mcp-test")
	if err := ensurePrivateFallbackRoot(root); err != nil {
		t.Fatalf("ensurePrivateFallbackRoot: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", root)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %o, want 0700", got)
	}
}

func TestEnsurePrivateFallbackRoot_ReusesOwnPrivateDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plect-mcp-test")
	if err := ensurePrivateFallbackRoot(root); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ensurePrivateFallbackRoot(root); err != nil {
		t.Fatalf("second call (reuse): %v", err)
	}
}

func TestEnsurePrivateFallbackRoot_RejectsExistingWorldReadableDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plect-mcp-test")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateFallbackRoot(root); err == nil {
		t.Fatal("want an error for a pre-existing 0755 directory, got nil")
	}
}

func TestEnsurePrivateFallbackRoot_RejectsSymlinkAtPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateFallbackRoot(link); err == nil {
		t.Fatal("want an error for a symlink at the root path, got nil")
	}
}

func TestCheckPrivateDirOwner_AcceptsCurrentUser(t *testing.T) {
	if err := checkPrivateDirOwner("/tmp/plect-mcp-test", uint32(os.Getuid())); err != nil {
		t.Errorf("checkPrivateDirOwner: %v, want nil for the caller's own uid", err)
	}
}

func TestCheckPrivateDirOwner_RejectsMismatchedOwner(t *testing.T) {
	other := uint32(os.Getuid()) + 1
	if err := checkPrivateDirOwner("/tmp/plect-mcp-test", other); err == nil {
		t.Fatal("want an error for a directory owned by a different uid, got nil")
	}
}

func TestResolveSessionSocket_FallbackHardensRootBeforeReturning(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	root := filepath.Join(t.TempDir(), "plect-mcp-test")

	got, err := resolveSessionSocket("owner/session", root)
	if err != nil {
		t.Fatalf("resolveSessionSocket: %v", err)
	}
	want := filepath.Join(root, "owner/session.sock")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	info, statErr := os.Stat(root)
	if statErr != nil {
		t.Fatalf("root was not created: %v", statErr)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("root mode = %o, want 0700 (hardening did not run)", mode)
	}
}

func TestResolveSessionSocket_FallbackPropagatesHardeningFailure(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	root := filepath.Join(t.TempDir(), "plect-mcp-test")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSessionSocket("owner/session", root); err == nil {
		t.Fatal("want an error when the fallback root fails hardening, got nil")
	}
}

func TestResolveSessionSocket_UnderXDGRuntimeDirSkipsHardening(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	unusedRoot := filepath.Join(t.TempDir(), "plect-mcp-test")

	got, err := resolveSessionSocket("owner/session", unusedRoot)
	if err != nil {
		t.Fatalf("resolveSessionSocket: %v", err)
	}
	want := "/run/user/1000/plect-mcp/owner/session.sock"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, statErr := os.Stat(unusedRoot); statErr == nil {
		t.Error("the unused fallback root should not have been created")
	}
}
