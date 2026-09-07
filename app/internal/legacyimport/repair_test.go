package legacyimport

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// buggyImportedFixture builds a legacy backup (see legacyFixture) plus a
// destDir holding a storage.db exactly as the pre-fix importer left it:
// every state.json session imported correctly, but orphan-events-only (an
// events/-only session) imported live as status="down" -- the bug
// RepairImportedSessions exists to fix on a host that already ran that
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
	return sourceDir, destDir
}

func TestRepairImportedSessions_DryRunReportsWithoutWriting(t *testing.T) {
	sourceDir, destDir := buggyImportedFixture(t)
	ctx := context.Background()

	report, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir, DryRun: true})
	if err != nil {
		t.Fatalf("RepairImportedSessions (dry-run): %v", err)
	}
	if report.WouldMark != 1 || report.AlreadyDestroyed != 0 || report.Kept != 2 {
		t.Errorf("report = %+v, want WouldMark=1 AlreadyDestroyed=0 Kept=2", report)
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

func TestRepairImportedSessions_MarksGhostsDestroyedAndBacksUpFirst(t *testing.T) {
	sourceDir, destDir := buggyImportedFixture(t)
	ctx := context.Background()

	report, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("RepairImportedSessions: %v", err)
	}
	if report.WouldMark != 1 || report.AlreadyDestroyed != 0 || report.Kept != 2 {
		t.Fatalf("report = %+v, want WouldMark=1 AlreadyDestroyed=0 Kept=2", report)
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
		t.Errorf("GetSession(orphan-events-only) = %+v, want nil (destroyed, hidden from live lookups)", ghost)
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
	var status, destroyedAt string
	if err := rawDB.QueryRowContext(ctx,
		`SELECT status, destroyed_at FROM sessions WHERE name = ?`, "orphan-events-only",
	).Scan(&status, &destroyedAt); err != nil {
		t.Fatalf("query orphan-events-only row: %v", err)
	}
	if status != "destroyed" {
		t.Errorf("status = %q, want destroyed", status)
	}
	if want := "2026-01-03T00:00:00.000000000Z"; destroyedAt != want {
		t.Errorf("destroyed_at = %q, want %q (its one event's time)", destroyedAt, want)
	}

	report2, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if report2.WouldMark != 0 || report2.AlreadyDestroyed != 1 || report2.Kept != 2 {
		t.Errorf("second run report = %+v, want WouldMark=0 AlreadyDestroyed=1 Kept=2 (idempotent)", report2)
	}
	if report2.BackupPath == report.BackupPath {
		t.Error("second run's backup path collided with the first's")
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

func TestRepairImportedSessions_ErrorsWhenTargetNeverImportedTheBackup(t *testing.T) {
	sourceDir, _, _ := legacyFixture(t)
	destDir := t.TempDir()
	ctx := context.Background()

	db, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err == nil {
		t.Fatal("RepairImportedSessions against a target that never imported --from's backup unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "never imported") {
		t.Errorf("error = %v, want it to mention the session was never imported (a --data-home mismatch, not an already-destroyed ghost)", err)
	}
}

// TestRepairImportedSessions_LeavesARecreatedLiveSessionAlone is the
// regression test for a real bug: a ghost name legitimately reused (e.g. an
// operator running `plect up orphan-events-only` between the buggy import
// and this repair) must not be destroyed just because it is absent from
// --from's state.json -- that absence is also true of every real, never
// pre-existing session name.
func TestRepairImportedSessions_LeavesARecreatedLiveSessionAlone(t *testing.T) {
	sourceDir, destDir := buggyImportedFixture(t)
	ctx := context.Background()

	db, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(time.Hour)
	if err := db.UpdateSession(ctx, "orphan-events-only", func(s *domain.Session) error {
		s.Status = contract.SessionStatusUp
		s.Workflow = "wf-real"
		s.UpdatedAt = later
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	report, err := RepairImportedSessions(ctx, RepairOptions{SourceDir: sourceDir, DestDir: destDir})
	if err != nil {
		t.Fatalf("RepairImportedSessions: %v", err)
	}
	if report.Recreated != 1 || report.WouldMark != 0 {
		t.Errorf("report = %+v, want Recreated=1 WouldMark=0", report)
	}

	db2, err := persistence.EnsureCurrent(ctx, persistence.PathIn(destDir))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	got, err := db2.GetSession(ctx, "orphan-events-only")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != contract.SessionStatusUp || got.Workflow != "wf-real" {
		t.Errorf("orphan-events-only = %+v, want still live, up, and untouched", got)
	}
}
