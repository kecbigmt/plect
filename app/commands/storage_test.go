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
	// storageMigrateAllowDevBuild is a package-level flag target: it outlives
	// this Execute() call, unlike the t.Setenv-backed env vars above, unless
	// reset explicitly (see execRoot's comment on configHomeFlag).
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

// seedRealBehindSchemaDatabase brings the database at fakeHome's default
// storage.db location to one migration short of the real embedded target,
// self-consistently (by actually running every earlier migration's real Up
// script), so the tests below exercise the guard against a genuinely
// behind-schema store rather than a fresh (schema-zero) one.
func seedRealBehindSchemaDatabase(t *testing.T, fakeHome string) {
	t.Helper()
	dbPath := persistence.PathIn(filepath.Join(fakeHome, ".local", "share", "plect"))
	if err := persistence.SeedWithMigrationsForTest(context.Background(), dbPath, persistence.RealMigrationsMinusLatestForTest()); err != nil {
		t.Fatalf("SeedWithMigrationsForTest: %v", err)
	}
}

// TestStorageMigrate_RefusesARealBehindSchemaDatabaseWithoutTheFlag is the
// regression test for a bug this guard shipped with: root's
// PersistentPreRunE ran the strict, non-allow-dev-build EnsureCurrent for
// every command, including `storage migrate` itself, so --allow-dev-build
// never had a chance to take effect against a real existing database.
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

// TestRootPersistentPreRun_RefusesARealBehindSchemaDatabaseForUnrelatedCommands
// confirms that skipping root's own currency check for `storage migrate`
// (the fix for the bug above) is scoped to that command alone: every other
// command still refuses against the same seeded database.
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
