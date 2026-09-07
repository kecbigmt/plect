package commands

import (
	"context"
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

func TestStorageMigrate_AllowDevBuildFlagStillCreatesAndReportsSchemaVersion(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Cleanup(func() { storageMigrateAllowDevBuild = false })

	out, err := execRoot(t, "storage", "migrate", "--allow-dev-build")
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

// seedRealBehindSchemaDatabase brings fakeHome's storage.db to one real
// migration short of target, so the guard sees a genuinely behind-schema
// store rather than a fresh one.
func seedRealBehindSchemaDatabase(t *testing.T, fakeHome string) {
	t.Helper()
	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if err := persistence.SeedWithMigrationsForTest(context.Background(), dbPath, persistence.RealMigrationsMinusLatestForTest()); err != nil {
		t.Fatalf("SeedWithMigrationsForTest: %v", err)
	}
}

func TestStorageMigrate_RefusesARealBehindSchemaDatabaseWithoutTheFlag(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	seedRealBehindSchemaDatabase(t, fakeHome)

	out, err := execRoot(t, "storage", "migrate")
	if err == nil {
		t.Fatalf("storage migrate against a real behind-schema database unexpectedly succeeded; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "development build") {
		t.Errorf("error = %q, want it to mention a development build", err)
	}
}

func TestStorageMigrate_AllowDevBuildFlagMigratesARealBehindSchemaDatabase(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Cleanup(func() { storageMigrateAllowDevBuild = false })
	seedRealBehindSchemaDatabase(t, fakeHome)

	out, err := execRoot(t, "storage", "migrate", "--allow-dev-build")
	if err != nil {
		t.Fatalf("storage migrate --allow-dev-build: %v; output:\n%s", err, out)
	}
	if !strings.Contains(out, "schema version") {
		t.Errorf("output = %q, want it to mention the resulting schema version", out)
	}
}

func TestRootPersistentPreRun_RefusesARealBehindSchemaDatabaseForUnrelatedCommands(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(confighome.EnvVar, "")
	t.Setenv(confighome.XDGEnvVar, "")
	seedRealBehindSchemaDatabase(t, fakeHome)

	out, err := execRoot(t, "config", "show")
	if err == nil {
		t.Fatalf("config show against a real behind-schema database unexpectedly succeeded; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "development build") {
		t.Errorf("error = %q, want it to mention a development build", err)
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

// TestStorageImport_DefaultDataHomeSucceedsWithoutDataHomeFlag is a
// command-level regression test: the documented default cutover
// (`plect storage import --from <backup>`, no --data-home) failed every
// time before this fix, because root's own PersistentPreRunE pre-created an
// empty storage.db at that same default path before storageImportCmd's RunE
// ever ran, and Run then refused to promote into a directory that already
// had one.
func TestStorageImport_DefaultDataHomeSucceedsWithoutDataHomeFlag(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(confighome.EnvVar, "")
	t.Setenv(confighome.XDGEnvVar, "")

	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "state.json"), []byte(`{"version":7,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := execRoot(t, "storage", "import", "--from", backupDir)
	if err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}
	if !strings.Contains(out, "promoted=true") {
		t.Errorf("output = %q, want it to report a completed promotion", out)
	}

	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if _, statErr := os.Stat(dbPath); statErr != nil {
		t.Fatalf("storage.db not created by the import: %v", statErr)
	}
}

// TestRootPersistentPreRun_DoesNotPreCreateStorageDBForStorageImport pins
// the fix directly: storageImportCmd must see no live database at the
// default path before its own RunE runs, the same carve-out
// storageMigrateCmd already had.
func TestRootPersistentPreRun_DoesNotPreCreateStorageDBForStorageImport(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(confighome.EnvVar, "")
	t.Setenv(confighome.XDGEnvVar, "")

	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "state.json"), []byte(`{"version":7,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A command that always fails its own RunE (missing --from) still runs
	// PersistentPreRunE first; if that pre-run created storage.db, it would
	// exist here even though the import itself never got a chance to run.
	if _, err := execRoot(t, "storage", "import"); err == nil {
		t.Fatal("storage import with no --from unexpectedly succeeded")
	}

	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("stat %s = %v, want not-exist (PersistentPreRunE must not pre-create it for storage import)", dbPath, statErr)
	}
}
