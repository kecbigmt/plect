package legacyimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
)

// RepairOptions configures one RepairImportedSessions call.
type RepairOptions struct {
	SourceDir string // the legacy backup an earlier `plect storage import` used
	DestDir   string // the plect data directory holding the storage.db to repair
	DryRun    bool   // report without writing (no backup, no destroy)
}

// RepairReport counts what one RepairImportedSessions call found and did.
type RepairReport struct {
	WouldMark        int // marked, or would be on DryRun
	AlreadyDestroyed int // a prior repair run, or a manual fix
	Kept             int // sessions state.json also names
	BackupPath       string
}

func (r *RepairReport) String() string {
	backup := r.BackupPath
	if backup == "" {
		backup = "(none: dry run)"
	}
	return fmt.Sprintf("would-mark=%d already-destroyed=%d kept=%d backup=%s", r.WouldMark, r.AlreadyDestroyed, r.Kept, backup)
}

// RepairImportedSessions is the one-time fix for a host that ran an older
// `plect storage import` before it marked an events/-only session destroyed
// (see Run): it marks each one destroyed here the same way, against the
// live storage.db in place, backing it up first (unless DryRun).
func RepairImportedSessions(ctx context.Context, opts RepairOptions) (*RepairReport, error) {
	report := &RepairReport{}

	if opts.SourceDir == "" || opts.DestDir == "" {
		return report, fmt.Errorf("legacyimport: SourceDir and DestDir are both required")
	}

	sf, err := readLegacyState(opts.SourceDir)
	if err != nil {
		return report, err
	}

	eventsRoot := filepath.Join(opts.SourceDir, "events")
	sessionNames, err := ListLegacySessionDirs(eventsRoot)
	if err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}

	dbPath := persistence.PathIn(opts.DestDir)
	db, err := persistence.EnsureCurrent(ctx, dbPath)
	if err != nil {
		return report, fmt.Errorf("legacyimport: open %s: %w", dbPath, err)
	}
	defer db.Close()

	if !opts.DryRun {
		backupPath, err := backupDatabase(ctx, db, dbPath)
		if err != nil {
			return report, fmt.Errorf("legacyimport: back up %s: %w", dbPath, err)
		}
		report.BackupPath = backupPath
	}

	for _, name := range sessionNames {
		if _, hasState := sf.Sessions[name]; hasState {
			report.Kept++
			continue
		}

		row, err := db.GetSession(ctx, name)
		if err != nil {
			return report, fmt.Errorf("legacyimport: get session %q: %w", name, err)
		}
		if row == nil {
			report.AlreadyDestroyed++
			continue
		}
		report.WouldMark++
		if opts.DryRun {
			continue
		}

		sl, err := ReadSessionDir(eventsRoot, name)
		if err != nil {
			return report, fmt.Errorf("legacyimport: %w", err)
		}
		destroyedAt := time.Now().UTC()
		if len(sl.Events) > 0 {
			destroyedAt = sl.Events[len(sl.Events)-1].Time
		}
		if err := db.DestroySession(ctx, name, destroyedAt); err != nil {
			return report, fmt.Errorf("legacyimport: mark %q destroyed: %w", name, err)
		}
	}

	return report, nil
}

// backupDatabase checkpoints db (so a copy of dbPath alone never strands
// pending writes in its WAL) then copies it and its WAL-mode siblings to a
// dated backup next to it, returning the backup's storage.db path.
func backupDatabase(ctx context.Context, db *persistence.DB, dbPath string) (string, error) {
	if err := db.Checkpoint(ctx); err != nil {
		return "", err
	}
	// Nanosecond precision so a quick re-run never collides with, and
	// silently overwrites, the backup it is trying to recover from.
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	backupPath := dbPath + ".backup-" + stamp
	if err := copyFileIfExists(dbPath, backupPath); err != nil {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := copyFileIfExists(dbPath+suffix, backupPath+suffix); err != nil {
			return "", err
		}
	}
	return backupPath, nil
}

func copyFileIfExists(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", src, err)
	}
	return atomicfile.Write(dst, data)
}
