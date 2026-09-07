package legacyimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
	contract "github.com/kecbigmt/plecture/contracts/state"
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
	Recreated        int // a live row exists but isn't the original ghost -- left alone
	BackupPath       string
}

func (r *RepairReport) String() string {
	backup := r.BackupPath
	if backup == "" {
		backup = "(none: dry run)"
	}
	return fmt.Sprintf("would-mark=%d already-destroyed=%d kept=%d recreated=%d backup=%s",
		r.WouldMark, r.AlreadyDestroyed, r.Kept, r.Recreated, backup)
}

// RepairImportedSessions is the one-time fix for a host that ran an older
// `plect storage import`, before it marked an events/-only session
// destroyed (see Run), against the live storage.db in place.
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
			everExisted, err := db.EventStreamIDsBySession(ctx, name)
			if err != nil {
				return report, fmt.Errorf("legacyimport: list incarnations for %q: %w", name, err)
			}
			if len(everExisted) == 0 {
				return report, fmt.Errorf("legacyimport: %q was never imported into %s; check --data-home", name, dbPath)
			}
			report.AlreadyDestroyed++
			continue
		}

		sl, err := ReadSessionDir(eventsRoot, name)
		if err != nil {
			return report, fmt.Errorf("legacyimport: %w", err)
		}
		if !isUntouchedGhost(row, sl) {
			// A real session (e.g. `plect up`) reused this name since the
			// buggy import -- destroying it would discard real work.
			report.Recreated++
			continue
		}

		report.WouldMark++
		if opts.DryRun {
			continue
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

// isUntouchedGhost reports whether row is still the exact placeholder Run's
// buggy predecessor created for name, not a later, unrelated incarnation.
// sl.GenID, when present, is authoritative -- ImportSession always reuses it
// as the row's id. Without one (a freshly minted id, indistinguishable by
// value alone), an untouched "down" placeholder's own shape is the only
// signal left.
func isUntouchedGhost(row *domain.Session, sl *SessionLog) bool {
	if sl != nil && sl.GenID != "" {
		return row.ID == sl.GenID
	}
	return row.Status == contract.SessionStatusDown && row.CreatedAt.Equal(row.UpdatedAt)
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
