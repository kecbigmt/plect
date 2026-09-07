package legacyimport

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
)

// RepairOptions configures one RepairImportedSessions call.
type RepairOptions struct {
	SourceDir string // the legacy backup an earlier `plect storage import` used
	DestDir   string // the plect data directory holding the storage.db to repair
	DryRun    bool   // report without writing (no backup, no delete)
}

// RepairReport counts what one RepairImportedSessions call found and did.
type RepairReport struct {
	WouldDelete int // deleted, or would be on DryRun
	Kept        int // sessions the backup's state.json also names
	BackupPath  string
}

func (r *RepairReport) String() string {
	backup := r.BackupPath
	if backup == "" {
		backup = "(none: dry run)"
	}
	return fmt.Sprintf("would-delete=%d kept=%d backup=%s", r.WouldDelete, r.Kept, backup)
}

// RepairImportedSessions is the one-time fix for a host that ran an
// importer version old enough to have materialized a row for an
// events/-only legacy session (see Run, which now skips those entirely):
// every session storage.db holds that the backup's state.json does not name
// is deleted outright, along with its events and every incarnation.
func RepairImportedSessions(ctx context.Context, opts RepairOptions) (*RepairReport, error) {
	report := &RepairReport{}

	if opts.SourceDir == "" || opts.DestDir == "" {
		return report, fmt.Errorf("legacyimport: SourceDir and DestDir are both required")
	}

	sf, err := readLegacyState(opts.SourceDir)
	if err != nil {
		return report, err
	}

	// A missing storage.db means --data-home names the wrong directory --
	// fail before EnsureCurrent below can silently mint an empty one.
	dbPath := persistence.PathIn(opts.DestDir)
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return report, fmt.Errorf("legacyimport: %s does not exist; --data-home must name an already-imported plect data directory", dbPath)
		}
		return report, fmt.Errorf("legacyimport: stat %s: %w", dbPath, statErr)
	}

	if !opts.DryRun {
		backupPath, err := backupDatabaseFiles(dbPath)
		if err != nil {
			return report, fmt.Errorf("legacyimport: back up %s: %w", dbPath, err)
		}
		report.BackupPath = backupPath
	}

	// EnsureCurrent may itself migrate the schema, so it opens only once
	// the backup above already exists to precede that write.
	db, err := persistence.EnsureCurrent(ctx, dbPath)
	if err != nil {
		return report, fmt.Errorf("legacyimport: open %s: %w", dbPath, err)
	}
	defer db.Close()

	names, err := db.EventStreamSessions(ctx)
	if err != nil {
		return report, fmt.Errorf("legacyimport: list sessions: %w", err)
	}

	for _, name := range names {
		if _, hasState := sf.Sessions[name]; hasState {
			report.Kept++
			continue
		}
		report.WouldDelete++
		if opts.DryRun {
			continue
		}
		if err := db.PurgeSessionByName(ctx, name); err != nil {
			return report, fmt.Errorf("legacyimport: delete %q: %w", name, err)
		}
	}

	return report, nil
}

// backupDatabaseFiles copies dbPath and its WAL-mode siblings, if any, to a
// dated backup next to it, called before dbPath is ever opened.
func backupDatabaseFiles(dbPath string) (string, error) {
	// Nanosecond precision: a quick re-run must never overwrite the very
	// backup it is trying to recover from.
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
