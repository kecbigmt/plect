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
