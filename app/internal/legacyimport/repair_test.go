package legacyimport

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// buggyImportedFixture builds a legacy backup (see legacyFixture) plus a
// destDir holding a storage.db exactly as the pre-fix importer left it:
// every state.json session imported correctly, but orphan-events-only (an
// events/-only session) imported live as status="down" -- the ghost row
// RepairImportedSessions exists to delete on a host that already ran that
// importer.
func buggyImportedFixture(t *testing.T) (sourceDir, destDir string) {
	t.Helper()
	sourceDir, _, _ = legacyFixture(t)
	destDir = t.TempDir()
	ctx := context.Background()

	db, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	for _, s := range []*domain.Session{
		{Name: "root-session", Workflow: "wf1", CreatedAt: now, UpdatedAt: now},
		{Name: "child-session", ParentSession: "root-session", Workflow: "wf1", CreatedAt: now, UpdatedAt: now},
		{Name: "orphan-events-only", Status: contract.SessionStatusDown, CreatedAt: now, UpdatedAt: now},
	} {
		if err := db.PutSession(ctx, s); err != nil {
			t.Fatalf("PutSession(%s): %v", s.Name, err)
		}
	}
	if _, err := db.AppendEvent(ctx, event.Event{
		ID: "01GHOSTEVT0000000000000001", SessionName: "orphan-events-only", Time: now,
		Type: "user.note", Direction: event.Internal,
	}); err != nil {
		t.Fatalf("AppendEvent(orphan-events-only): %v", err)
	}
	return sourceDir, destDir
}

func TestRepairImportedSessions_DryRunReportsWithoutWriting(t *testing.T) {
	sourceDir, destDir := buggyImportedFixture(t)
	ctx := context.Background()

	report, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir, DryRun: true})
	if err != nil {
		t.Fatalf("RepairImportedSessions (dry-run): %v", err)
	}
	if report.WouldDelete != 1 || report.Kept != 2 {
		t.Errorf("report = %+v, want WouldDelete=1 Kept=2", report)
	}
	if report.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty for a dry run", report.BackupPath)
	}

	db, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	defer db.Close()
	ghost, err := db.GetSession(ctx, "orphan-events-only")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if ghost == nil || ghost.Status != contract.SessionStatusDown {
		t.Errorf("orphan-events-only = %+v, want still live and down (dry run must not write)", ghost)
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir(destDir): %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "storage.db.backup-") {
			t.Errorf("dry run left a backup file %q behind", e.Name())
		}
	}
}

// TestRepairImportedSessions_DryRunCreatesNoGateOrBackupFiles is the
// regression test for a real bug: DryRun read via persistence.Open/
// EnsureCurrent, both of which create this package's own
// .access.lock/.coordination.lock gate sidecars (EnsureCurrent can also
// migrate the schema) as a side effect of opening -- filesystem mutation a
// dry run's own "reports without writing anything" promise must not have.
// listSessionNamesReadOnly's immutable=1 connection also skips the -wal/-shm
// a plain mode=ro one would still create, so the comparison below is exact.
func TestRepairImportedSessions_DryRunCreatesNoGateOrBackupFiles(t *testing.T) {
	sourceDir, destDir := buggyImportedFixture(t)
	ctx := context.Background()

	before, err := direntNames(destDir)
	if err != nil {
		t.Fatalf("ReadDir(destDir) before: %v", err)
	}

	if _, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir, DryRun: true}); err != nil {
		t.Fatalf("RepairImportedSessions (dry-run): %v", err)
	}

	after, err := direntNames(destDir)
	if err != nil {
		t.Fatalf("ReadDir(destDir) after: %v", err)
	}
	if !slices.Equal(before, after) {
		t.Errorf("destDir contents changed by a dry run:\nbefore=%v\nafter= %v", before, after)
	}
}

func direntNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	slices.Sort(names)
	return names, nil
}

func TestRepairImportedSessions_DeletesGhostsAndBacksUpFirst(t *testing.T) {
	sourceDir, destDir := buggyImportedFixture(t)
	ctx := context.Background()

	report, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("RepairImportedSessions: %v", err)
	}
	if report.WouldDelete != 1 || report.Kept != 2 {
		t.Fatalf("report = %+v, want WouldDelete=1 Kept=2", report)
	}
	if report.BackupPath == "" {
		t.Fatal("BackupPath empty, want a dated backup path for a real run")
	}
	if _, err := os.Stat(report.BackupPath); err != nil {
		t.Fatalf("stat backup: %v", err)
	}

	db, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	defer db.Close()

	ghost, err := db.GetSession(ctx, "orphan-events-only")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if ghost != nil {
		t.Errorf("GetSession(orphan-events-only) = %+v, want nil (deleted)", ghost)
	}
	all, err := db.AllSessions(ctx)
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("AllSessions = %d, want 2 (only root-session and child-session live)", len(all))
	}

	rawDB, err := sql.Open("sqlite3", persistence.PathIn(destDir))
	if err != nil {
		t.Fatalf("open storage.db directly: %v", err)
	}
	defer rawDB.Close()
	var sessionCount, eventCount int
	if err := rawDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE name = ?`, "orphan-events-only").Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Error("orphan-events-only's sessions row still present, want fully deleted")
	}
	if err := rawDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM events e JOIN sessions s ON e.session_id = s.id WHERE s.name = ?`, "orphan-events-only").Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 0 {
		t.Error("orphan-events-only's events still present, want fully deleted")
	}

	report2, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if report2.WouldDelete != 0 || report2.Kept != 2 {
		t.Errorf("second run report = %+v, want WouldDelete=0 Kept=2 (idempotent)", report2)
	}
	if report2.BackupPath == report.BackupPath {
		t.Error("second run's backup path collided with the first's")
	}
}

// TestRepairImportedSessions_DeletesAnyNameAbsentFromTheBackupEvenIfLive is
// the acceptance-driving corollary of the amendment's literal rule (every
// session storage.db holds that the backup's state.json does not name is
// deleted): a session created after cutover with real work and no relation
// to the buggy import at all is deleted the same way a genuine ghost is,
// because presence in the backup's state.json is the only criterion.
func TestRepairImportedSessions_DeletesAnyNameAbsentFromTheBackupEvenIfLive(t *testing.T) {
	sourceDir, destDir := buggyImportedFixture(t)
	ctx := context.Background()

	db, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: "post-cutover-real", Status: contract.SessionStatusUp, Workflow: "wf-real", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	report, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("RepairImportedSessions: %v", err)
	}
	if report.WouldDelete != 2 || report.Kept != 2 {
		t.Errorf("report = %+v, want WouldDelete=2 Kept=2", report)
	}

	db2, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	got, err := db2.GetSession(ctx, "post-cutover-real")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("GetSession(post-cutover-real) = %+v, want nil (deleted along with the ghost)", got)
	}
}

func TestRepairImportedSessions_RequiresSourceAndDestDir(t *testing.T) {
	if _, err := RepairImportedSessions(context.Background(), RepairOptions{}); err == nil {
		t.Fatal("RepairImportedSessions with no SourceDir/DestDir must fail")
	}
}

// TestRepairImportedSessions_BacksUpEvenWhenOpeningTheDatabaseFails is the
// regression test for a real bug: RepairImportedSessions called
// EnsureCurrent (which migrates as a side effect of opening) before the
// backup step, so a schema migration -- or here, EnsureCurrent's own
// dev-build refusal against a behind-schema database -- could land with no
// backup covering it. The backup must exist and be untouched even when
// opening the target afterward fails outright.
func TestRepairImportedSessions_BacksUpEvenWhenOpeningTheDatabaseFails(t *testing.T) {
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "state.json"), []byte(`{"version":7,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	ctx := context.Background()
	dbPath := persistence.PathIn(destDir)

	if err := persistence.SeedWithMigrationsForTest(ctx, dbPath, persistence.RealMigrationsMinusLatestForTest()); err != nil {
		t.Fatalf("seed behind-schema db: %v", err)
	}
	behindDB, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	behindVersion, err := behindDB.Version(ctx)
	behindDB.Close()
	if err != nil {
		t.Fatalf("read behind-schema version: %v", err)
	}

	report, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("RepairImportedSessions against a behind-schema database in a dev-build test binary unexpectedly succeeded")
	}
	if report.BackupPath == "" {
		t.Fatal("BackupPath empty despite the open/migrate failure -- the backup must be taken before EnsureCurrent is ever called")
	}

	backupDB, err := persistence.Open(report.BackupPath)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	backupVersion, err := backupDB.Version(ctx)
	backupDB.Close()
	if err != nil {
		t.Fatalf("read backup version: %v", err)
	}
	if backupVersion != behindVersion {
		t.Errorf("backup schema version = %d, want %d (its pre-open state)", backupVersion, behindVersion)
	}
}

// TestRepairImportedSessions_DryRunDoesNotMigrateTheDatabase is the
// regression test for a real bug: DryRun called EnsureCurrent the same as a
// real run, so it could migrate the schema even though it documents itself
// as reporting "without writing anything."
func TestRepairImportedSessions_DryRunDoesNotMigrateTheDatabase(t *testing.T) {
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "state.json"), []byte(`{"version":7,"sessions":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	ctx := context.Background()
	dbPath := persistence.PathIn(destDir)

	if err := persistence.SeedWithMigrationsForTest(ctx, dbPath, persistence.RealMigrationsMinusLatestForTest()); err != nil {
		t.Fatalf("seed behind-schema db: %v", err)
	}
	behindDB, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	behindVersion, err := behindDB.Version(ctx)
	behindDB.Close()
	if err != nil {
		t.Fatalf("read behind-schema version: %v", err)
	}

	if _, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir, DryRun: true}); err != nil {
		t.Fatalf("RepairImportedSessions (dry-run): %v", err)
	}

	afterDB, err := persistence.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	afterVersion, err := afterDB.Version(ctx)
	afterDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	if afterVersion != behindVersion {
		t.Errorf("schema version after dry run = %d, want unchanged %d", afterVersion, behindVersion)
	}
}
