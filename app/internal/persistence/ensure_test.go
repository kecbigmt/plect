package persistence

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// migrationFixture builds an in-memory goose migration source so tests can
// exercise interrupted, concurrent, and version-mismatched migrations
// without touching the real embedded migrations/ tree.
func migrationFixture(files map[string]string) fstest.MapFS {
	fsys := make(fstest.MapFS, len(files))
	for name, sql := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(sql)}
	}
	return fsys
}

const migrationA = `-- +goose Up
CREATE TABLE a (id INTEGER PRIMARY KEY);

-- +goose Down
DROP TABLE a;
`

const migrationBOK = `-- +goose Up
CREATE TABLE b (id INTEGER PRIMARY KEY);

-- +goose Down
DROP TABLE b;
`

// migrationBBroken references a table that does not exist, so it always
// fails deterministically without depending on constraint timing.
const migrationBBroken = `-- +goose Up
ALTER TABLE does_not_exist ADD COLUMN x TEXT;

-- +goose Down
SELECT 1;
`

func testDBPath(t *testing.T) string {
	t.Helper()
	return PathIn(t.TempDir())
}

func TestEnsureCurrent_FreshDatabaseMigratesToTarget(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	fsys := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	db, err := ensureCurrent(ctx, path, fsys, false, false)
	if err != nil {
		t.Fatalf("ensureCurrent: %v", err)
	}
	defer db.Close()

	current, target, err := db.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target || current != 2 {
		t.Fatalf("version = (%d, %d), want (2, 2)", current, target)
	}

	marker, err := readMarker(newAccessGate(path).markerPath)
	if err != nil {
		t.Fatalf("readMarker: %v", err)
	}
	if marker != nil {
		t.Errorf("marker = %+v, want none left behind after a clean migration", marker)
	}
}

func TestEnsureCurrent_AlreadyCurrentReturnsWithoutError(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	fsys := migrationFixture(map[string]string{"00001_a.sql": migrationA})

	first, err := ensureCurrent(ctx, path, fsys, false, false)
	if err != nil {
		t.Fatalf("first ensureCurrent: %v", err)
	}
	first.Close()

	second, err := ensureCurrent(ctx, path, fsys, false, false)
	if err != nil {
		t.Fatalf("second ensureCurrent: %v", err)
	}
	defer second.Close()

	current, target, err := second.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target {
		t.Errorf("current = %d, target = %d, want equal on a database already at the latest version", current, target)
	}
}

func TestEnsureCurrent_DevBuildRefusesToMigrateAnExistingDatabaseBehindSchema(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	behind := migrationFixture(map[string]string{"00001_a.sql": migrationA})
	ahead := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	seed, err := ensureCurrent(ctx, path, behind, false, false)
	if err != nil {
		t.Fatalf("seed ensureCurrent: %v", err)
	}
	seed.Close()

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded database: %v", err)
	}

	db, err := ensureCurrent(ctx, path, ahead, true, false)
	if err == nil {
		db.Close()
		t.Fatal("ensureCurrent for a dev build against an existing database behind schema unexpectedly succeeded")
	}
	if db != nil {
		t.Errorf("db = %v, want nil on refusal", db)
	}
	for _, want := range []string{"development build", "schema 1", "migrate it to 2", "--allow-dev-build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database after refusal: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("database file changed after a dev-build refusal, want byte-identical")
	}

	marker, merr := readMarker(newAccessGate(path).markerPath)
	if merr != nil {
		t.Fatalf("readMarker: %v", merr)
	}
	if marker != nil {
		t.Errorf("marker = %+v, want none: a dev-build refusal must never become a migrator", marker)
	}
}

func TestEnsureCurrent_DevBuildWithAllowDevBuildMigratesAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	behind := migrationFixture(map[string]string{"00001_a.sql": migrationA})
	ahead := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	seed, err := ensureCurrent(ctx, path, behind, false, false)
	if err != nil {
		t.Fatalf("seed ensureCurrent: %v", err)
	}
	seed.Close()

	db, err := ensureCurrent(ctx, path, ahead, true, true)
	if err != nil {
		t.Fatalf("ensureCurrent with allowDevBuild: %v", err)
	}
	defer db.Close()

	current, target, err := db.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target || current != 2 {
		t.Fatalf("version = (%d, %d), want (2, 2)", current, target)
	}
}

func TestEnsureCurrent_DevBuildCreatingAFreshDatabaseNeedsNoFlag(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	fsys := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	db, err := ensureCurrent(ctx, path, fsys, true, false)
	if err != nil {
		t.Fatalf("ensureCurrent for a dev build creating a fresh database: %v", err)
	}
	defer db.Close()

	current, target, err := db.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target || current != 2 {
		t.Fatalf("version = (%d, %d), want (2, 2): a database this call creates carries none of the incident's risk", current, target)
	}
}

func TestEnsureCurrent_ReleaseBuildMigratesAnExistingDatabaseAutomatically(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	behind := migrationFixture(map[string]string{"00001_a.sql": migrationA})
	ahead := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	seed, err := ensureCurrent(ctx, path, behind, false, false)
	if err != nil {
		t.Fatalf("seed ensureCurrent: %v", err)
	}
	seed.Close()

	db, err := ensureCurrent(ctx, path, ahead, false, false)
	if err != nil {
		t.Fatalf("ensureCurrent for a release build against an existing database behind schema: %v", err)
	}
	defer db.Close()

	current, target, err := db.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target || current != 2 {
		t.Fatalf("version = (%d, %d), want (2, 2): a release build migrates automatically", current, target)
	}
}

func TestEnsureCurrent_RefusesAndDoesNotModifyADatabaseNewerThanSupported(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)

	newer, err := ensureCurrent(ctx, path, migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK}), false, false)
	if err != nil {
		t.Fatalf("migrate to newer schema: %v", err)
	}
	newer.Close()

	older := migrationFixture(map[string]string{"00001_a.sql": migrationA})
	db, err := ensureCurrent(ctx, path, older, false, false)
	if err == nil {
		db.Close()
		t.Fatal("ensureCurrent with an older migration set unexpectedly succeeded")
	}
	if db != nil {
		t.Errorf("db = %v, want nil on refusal", db)
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error = %q, want an actionable newer-than-supported message", err)
	}

	verify, verr := Open(path)
	if verr != nil {
		t.Fatalf("Open for verification: %v", verr)
	}
	defer verify.Close()
	verify.migrations = older
	current, _, verr := verify.version(ctx)
	if verr != nil {
		t.Fatalf("version: %v", verr)
	}
	if current != 2 {
		t.Errorf("ledger version = %d after refusal, want unchanged at 2", current)
	}

	marker, merr := readMarker(newAccessGate(path).markerPath)
	if merr != nil {
		t.Fatalf("readMarker: %v", merr)
	}
	if marker != nil {
		t.Errorf("marker = %+v, want none: refusal must never become a migrator", marker)
	}
}

func TestEnsureCurrent_InterruptedMigrationPreservesEvidenceAndResumesAfterFix(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	broken := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBBroken})

	db, err := ensureCurrent(ctx, path, broken, false, false)
	if err == nil {
		db.Close()
		t.Fatal("ensureCurrent with a broken second migration unexpectedly succeeded")
	}
	if db != nil {
		t.Errorf("db = %v, want nil on migration failure", db)
	}

	gate := newAccessGate(path)
	marker, merr := readMarker(gate.markerPath)
	if merr != nil {
		t.Fatalf("readMarker: %v", merr)
	}
	if marker == nil {
		t.Fatal("marker = nil, want failure evidence preserved")
	}
	if marker.Stage != "failed" {
		t.Errorf("marker.Stage = %q, want %q", marker.Stage, "failed")
	}
	if marker.Error == "" {
		t.Error("marker.Error is empty, want the underlying migration failure recorded")
	}

	verify, verr := Open(path)
	if verr != nil {
		t.Fatalf("Open for verification: %v", verr)
	}
	verify.migrations = broken
	current, _, verr := verify.version(ctx)
	if verr != nil {
		t.Fatalf("version: %v", verr)
	}
	if current != 1 {
		t.Errorf("ledger version after failed migration = %d, want 1 (only the first migration committed)", current)
	}
	var tableCount int
	if serr := verify.write.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='b'").Scan(&tableCount); serr != nil {
		t.Fatalf("check table b: %v", serr)
	}
	if tableCount != 0 {
		t.Errorf("table b exists after a failed migration that should have rolled back atomically")
	}
	verify.Close()

	fixed := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})
	resumed, err := ensureCurrent(ctx, path, fixed, false, false)
	if err != nil {
		t.Fatalf("ensureCurrent after fixing the migration: %v", err)
	}
	defer resumed.Close()

	current, target, verr := resumed.version(ctx)
	if verr != nil {
		t.Fatalf("version: %v", verr)
	}
	if current != target || current != 2 {
		t.Fatalf("version after resuming = (%d, %d), want (2, 2)", current, target)
	}

	marker, merr = readMarker(gate.markerPath)
	if merr != nil {
		t.Fatalf("readMarker after successful resume: %v", merr)
	}
	if marker != nil {
		t.Errorf("marker = %+v, want removed after the resumed migration succeeded", marker)
	}
}

func TestMigrate_WritesMarkerBeforeWaitingForAccessExclusive(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	db.migrations = migrationFixture(map[string]string{"00001_a.sql": migrationA})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (to version 1): %v", err)
	}
	db.migrations = migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	gate := newAccessGate(path)
	unlockAccessShared, err := gate.accessShared()
	if err != nil {
		t.Fatalf("accessShared: %v", err)
	}

	migrateErr := make(chan error, 1)
	go func() {
		migrateErr <- db.Migrate(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var marker *migrationMarker
	for time.Now().Before(deadline) {
		marker, err = readMarker(gate.markerPath)
		if err != nil {
			t.Fatalf("readMarker: %v", err)
		}
		if marker != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if marker == nil {
		t.Fatal("marker was not written while Migrate was still waiting for the access lock")
	}
	if marker.Stage != "migrating" {
		t.Errorf("marker.Stage = %q, want %q", marker.Stage, "migrating")
	}
	if marker.PID != os.Getpid() {
		t.Errorf("marker.PID = %d, want %d", marker.PID, os.Getpid())
	}

	unlockAccessShared()

	if err := <-migrateErr; err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	current, target, err := db.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target || current != 2 {
		t.Fatalf("version after Migrate = (%d, %d), want (2, 2)", current, target)
	}
}

// Several processes pinging the same not-yet-existent database file for the
// first time race on creating its WAL and shared-memory sidecars; Open's
// coordination-lock guard (see open.go) is what prevents that.
func TestEnsureCurrent_ConcurrentFreshOpenNeverCollides(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	fsys := migrationFixture(map[string]string{"00001_a.sql": migrationA})

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	dbs := make([]*DB, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbs[i], errs[i] = ensureCurrent(ctx, path, fsys, false, false)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: ensureCurrent: %v", i, err)
		}
		if dbs[i] != nil {
			dbs[i].Close()
		}
	}

	verify, err := Open(path)
	if err != nil {
		t.Fatalf("Open for verification: %v", err)
	}
	defer verify.Close()
	verify.migrations = fsys
	current, target, err := verify.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target || current != 1 {
		t.Fatalf("version after concurrent fresh open = (%d, %d), want (1, 1)", current, target)
	}
}

func TestEnsureCurrent_ConcurrentStartupOnlyOneMigratesAndBothSucceed(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	fsys := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	// Bring the database to version 1 first, uncontended, so this test
	// isolates the scenario the acceptance criteria describe — several
	// processes racing to apply a pending migration to an already-existing
	// database — from the fresh-open race
	// TestEnsureCurrent_ConcurrentFreshOpenNeverCollides covers separately.
	seed, err := ensureCurrent(ctx, path, migrationFixture(map[string]string{"00001_a.sql": migrationA}), false, false)
	if err != nil {
		t.Fatalf("seed ensureCurrent: %v", err)
	}
	seed.Close()

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	dbs := make([]*DB, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbs[i], errs[i] = ensureCurrent(ctx, path, fsys, false, false)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: ensureCurrent: %v", i, err)
		}
		if dbs[i] != nil {
			dbs[i].Close()
		}
	}

	verify, err := Open(path)
	if err != nil {
		t.Fatalf("Open for verification: %v", err)
	}
	defer verify.Close()
	verify.migrations = fsys
	current, target, err := verify.version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if current != target || current != 2 {
		t.Fatalf("version after concurrent startup = (%d, %d), want (2, 2): every process must observe the fully migrated schema, never a half-migrated one", current, target)
	}
}

func TestAccessGate_EnterSharedWaitsForCoordinationLockRatherThanRefusingImmediately(t *testing.T) {
	path := testDBPath(t)
	gate := newAccessGate(path)

	unlock, ok, err := tryFlockPath(gate.coordinationLockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if !ok {
		t.Fatal("tryFlockPath did not acquire the uncontended coordination lock")
	}

	const hold = 300 * time.Millisecond
	time.AfterFunc(hold, unlock)

	start := time.Now()
	unlockAccess, err := gate.enterShared(context.Background())
	if err != nil {
		t.Fatalf("enterShared: %v", err)
	}
	defer unlockAccess()
	elapsed := time.Since(start)
	if elapsed < hold/2 {
		t.Errorf("enterShared returned after %s, want it blocked for roughly %s while the coordination lock was held", elapsed, hold)
	}
}

func TestAccessGate_MigrationInProgressErrorReportsMarkerWhenPresent(t *testing.T) {
	path := testDBPath(t)
	gate := newAccessGate(path)

	withoutMarker := gate.migrationInProgressError()
	if withoutMarker == nil {
		t.Fatal("migrationInProgressError() = nil, want a non-nil error")
	}
	if strings.Contains(withoutMarker.Error(), "pid") {
		t.Errorf("error without a marker unexpectedly mentions a pid: %q", withoutMarker)
	}

	if err := writeMarker(gate.markerPath, migrationMarker{PID: 4242, Stage: "migrating", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}

	withMarker := gate.migrationInProgressError()
	if !strings.Contains(withMarker.Error(), "4242") {
		t.Errorf("error = %q, want it to mention the marker's recorded pid 4242", withMarker)
	}
	if !strings.Contains(withMarker.Error(), "migrating") {
		t.Errorf("error = %q, want it to mention the marker's recorded stage", withMarker)
	}
}
