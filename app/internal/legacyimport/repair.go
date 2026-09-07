package legacyimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kecbigmt/plecture/app/internal/legacystate"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
)

// RepairOptions configures one RepairImportedSessions call.
type RepairOptions struct {
	// SourceDir's state.json and events/ tree together decide which names
	// are ghosts: storage.db alone cannot tell a ghost row from a real one.
	SourceDir string
	DestDir   string // the plect data directory holding the storage.db to repair
	DryRun    bool   // previews via persistence.ReadSessionNames, skipping backup and delete entirely
}

// RepairReport counts what one RepairImportedSessions call found and did.
type RepairReport struct {
	WouldDelete int
	Kept        int
	NotInBackup int
	BackupPath  string
}

func (r *RepairReport) String() string {
	backup := r.BackupPath
	if backup == "" {
		backup = "(none: dry run)"
	}
	return fmt.Sprintf("would-delete=%d kept=%d not-in-backup=%d backup=%s", r.WouldDelete, r.Kept, r.NotInBackup, backup)
}

// RepairImportedSessions is the one-time fix for a host that ran an
// importer old enough to still materialize a row for an events/-only
// legacy session (see Run): every such row is deleted outright.
func RepairImportedSessions(ctx context.Context, opts RepairOptions) (*RepairReport, error) {
	report := &RepairReport{}

	if opts.SourceDir == "" || opts.DestDir == "" {
		return report, fmt.Errorf("legacyimport: SourceDir and DestDir are both required")
	}

	sf, err := readLegacyState(opts.SourceDir)
	if err != nil {
		return report, err
	}
	classify, err := newSessionClassifier(opts.SourceDir, sf)
	if err != nil {
		return report, err
	}

	// A missing storage.db means --data-home is wrong; fail before EnsureCurrent mints an empty one.
	dbPath := persistence.PathIn(opts.DestDir)
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return report, fmt.Errorf("legacyimport: %s does not exist; --data-home must name an already-imported plect data directory", dbPath)
		}
		return report, fmt.Errorf("legacyimport: stat %s: %w", dbPath, statErr)
	}

	if opts.DryRun {
		names, err := persistence.ReadSessionNames(ctx, dbPath)
		if err != nil {
			return report, fmt.Errorf("legacyimport: %w", err)
		}
		for _, name := range names {
			tallyClassification(report, classify(name))
		}
		return report, nil
	}

	backupPath, err := backupDatabaseFiles(dbPath)
	if err != nil {
		return report, fmt.Errorf("legacyimport: back up %s: %w", dbPath, err)
	}
	report.BackupPath = backupPath

	// Opens only once the backup above exists, so EnsureCurrent's own
	// migration can never land ahead of it.
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
		if class := classify(name); class == classGhost {
			report.WouldDelete++
			if err := db.PurgeSessionByName(ctx, name); err != nil {
				return report, fmt.Errorf("legacyimport: delete %q: %w", name, err)
			}
		} else {
			tallyClassification(report, class)
		}
	}

	return report, nil
}

type sessionClassification int

const (
	classKept sessionClassification = iota
	classGhost
	classNotInBackup
)

func tallyClassification(report *RepairReport, class sessionClassification) {
	switch class {
	case classKept:
		report.Kept++
	case classGhost:
		report.WouldDelete++
	case classNotInBackup:
		report.NotInBackup++
	}
}

// A name missing from state.json is ambiguous on its own: a genuine ghost
// and a session created after the backup was taken are both missing from
// it, for opposite reasons. The events/ tree resolves the ambiguity, since
// only a ghost still has a directory there.
func newSessionClassifier(sourceDir string, sf *legacystate.StateFile) (func(name string) sessionClassification, error) {
	eventsRoot := filepath.Join(sourceDir, "events")
	eventDirNames, err := ListLegacySessionDirs(eventsRoot)
	if err != nil {
		return nil, fmt.Errorf("legacyimport: %w", err)
	}
	ghostNames := make(map[string]bool, len(eventDirNames))
	for _, name := range eventDirNames {
		if _, hasState := sf.Sessions[name]; !hasState {
			ghostNames[name] = true
		}
	}
	return func(name string) sessionClassification {
		if _, hasState := sf.Sessions[name]; hasState {
			return classKept
		}
		if ghostNames[name] {
			return classGhost
		}
		return classNotInBackup
	}, nil
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
