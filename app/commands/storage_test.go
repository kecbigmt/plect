package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/persistence"
)

func TestStorageMigrate_CreatesAndReportsSchemaVersion(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")

	out, err := execRoot(t, "storage", "migrate")
	if err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}

	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("storage.db not created at %s: %v", dbPath, statErr)
	}
	if !strings.Contains(out, "schema version") {
		t.Errorf("output = %q, want it to mention the resulting schema version", out)
	}
}

// TestStorageMigrate_CreatesTheLiterallyNamedStorageFiles pins the exact
// on-disk file names by literal, independent of persistence.PathIn: every
// other test in this file resolves its expected path through PathIn, so a
// regression in PathIn itself (or the fileName constant it derives from)
// would go unnoticed rather than being caught here.
func TestStorageMigrate_CreatesTheLiterallyNamedStorageFiles(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")

	if out, err := execRoot(t, "storage", "migrate"); err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}

	dataDir := filepath.Join(fakeHome, ".local", "share", "plect")
	for _, name := range []string{"storage.db", "storage.db.access.lock", "storage.db.coordination.lock"} {
		if _, statErr := os.Stat(filepath.Join(dataDir, name)); statErr != nil {
			t.Errorf("%s not created: %v", name, statErr)
		}
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dataDir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "store.db") {
			t.Errorf("legacy store.db artifact present in a fresh data dir: %s", entry.Name())
		}
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

func TestRootPersistentPreRun_CreatesStorageDBForEveryCommand(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(confighome.EnvVar, "")
	t.Setenv(confighome.XDGEnvVar, "")

	if out, err := execRoot(t, "config", "show"); err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}

	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("storage.db not created by an unrelated command's PersistentPreRunE: %v", statErr)
	}
}
