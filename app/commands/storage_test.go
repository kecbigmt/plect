package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	contract "github.com/kecbigmt/plecture/contracts/state"
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

// TestStorageImport_DefaultDataHomeSucceedsWithoutDataHomeFlag: the
// documented default cutover used to always refuse itself here (root's own
// PersistentPreRunE pre-created an empty storage.db at this same path).
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

// TestRootPersistentPreRun_DoesNotPreCreateStorageDBForStorageImport: the
// same pre-run carve-out storageMigrateCmd already had.
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

	// Missing --from fails RunE, but PersistentPreRunE still runs first.
	if _, err := execRoot(t, "storage", "import"); err == nil {
		t.Fatal("storage import with no --from unexpectedly succeeded")
	}

	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("stat %s = %v, want not-exist (PersistentPreRunE must not pre-create it for storage import)", dbPath, statErr)
	}
}

// TestRootPersistentPreRun_DoesNotPreCreateStorageDBForStorageRepair: the
// same pre-run carve-out storage import already has -- repair must back up
// --data-home's own storage.db before anything opens or migrates it, and
// this hook opening the default path first would risk exactly that.
func TestRootPersistentPreRun_DoesNotPreCreateStorageDBForStorageRepair(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(confighome.EnvVar, "")
	t.Setenv(confighome.XDGEnvVar, "")

	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "state.json"), []byte(`{"version":7,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Missing --from fails RunE, but PersistentPreRunE still runs first.
	if _, err := execRoot(t, "storage", "repair-imported-sessions"); err == nil {
		t.Fatal("storage repair-imported-sessions with no --from unexpectedly succeeded")
	}

	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("stat %s = %v, want not-exist (PersistentPreRunE must not pre-create it for storage repair)", dbPath, statErr)
	}
}

// TestStorageRepairImportedSessions_DeletesGhosts exercises the CLI wiring
// end to end against a storage.db holding a ghost row (a name absent from
// the legacy backup's state.json) left behind by a pre-fix importer.
func TestStorageRepairImportedSessions_DeletesGhosts(t *testing.T) {
	t.Cleanup(func() { storageRepairDryRun = false })
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(confighome.EnvVar, "")
	t.Setenv(confighome.XDGEnvVar, "")

	backupDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDir, "state.json"), []byte(`{"version":7,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	db, err := persistence.EnsureCurrent(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("seed storage.db: %v", err)
	}
	now := time.Now().UTC()
	if err := db.PutSession(context.Background(), &domain.Session{Name: "ghost-session", Status: contract.SessionStatusDown, CreatedAt: now, UpdatedAt: now}); err != nil {
		db.Close()
		t.Fatalf("seed ghost-session: %v", err)
	}
	db.Close()

	dryOut, err := execRoot(t, "storage", "repair-imported-sessions", "--from", backupDir, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v; output:\n%s", err, dryOut)
	}
	if !strings.Contains(dryOut, "would-delete=1") {
		t.Errorf("dry-run output = %q, want it to report would-delete=1", dryOut)
	}
	// cobra flag bindings outlive a single Execute() call (see execRoot's own
	// doc comment on --config-home), so the real run below must explicitly
	// reset the bool the dry run just set to true.
	storageRepairDryRun = false

	out, err := execRoot(t, "storage", "repair-imported-sessions", "--from", backupDir)
	if err != nil {
		t.Fatalf("Execute() error = %v; output:\n%s", err, out)
	}
	if !strings.Contains(out, "would-delete=1") {
		t.Errorf("output = %q, want it to report would-delete=1", out)
	}

	db, err = persistence.EnsureCurrent(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen storage.db: %v", err)
	}
	defer db.Close()
	ghost, err := db.GetSession(context.Background(), "ghost-session")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if ghost != nil {
		t.Errorf("GetSession(ghost-session) = %+v, want nil (deleted)", ghost)
	}
}
