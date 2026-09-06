package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/confighome"
)

func TestStorageMigrate_CreatesAndReportsSchemaVersion(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")

	out, err := execRoot(t, "storage", "migrate")
	if err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}

	dbPath := filepath.Join(fakeHome, ".local", "share", "plect", "store.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("store.db not created at %s: %v", dbPath, statErr)
	}
	if !strings.Contains(out, "schema version") {
		t.Errorf("output = %q, want it to mention the resulting schema version", out)
	}
}

func TestStorageMigrate_IsANoOpOnASecondRun(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")

	first, err := execRoot(t, "storage", "migrate")
	if err != nil {
		t.Fatalf("first migrate: %v; output:\n%s", err, first)
	}
	second, err := execRoot(t, "storage", "migrate")
	if err != nil {
		t.Fatalf("second migrate: %v; output:\n%s", err, second)
	}
	if !strings.Contains(second, "schema version") {
		t.Errorf("second run output = %q, want it to still report the schema version", second)
	}
}

func TestRootPersistentPreRun_CreatesStoreDBForEveryCommand(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(confighome.EnvVar, "")
	t.Setenv(confighome.XDGEnvVar, "")

	if out, err := execRoot(t, "config", "show"); err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}

	dbPath := filepath.Join(fakeHome, ".local", "share", "plect", "store.db")
	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("store.db not created by an unrelated command's PersistentPreRunE: %v", statErr)
	}
}
