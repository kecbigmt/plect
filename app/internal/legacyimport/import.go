package legacyimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/legacystate"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// Options configures one Run call.
type Options struct {
	// SourceDir is a stopped-writer backup of plect's legacy data directory:
	// state.json, the events/ tree, pending_delivery.json, delivery-locks/.
	SourceDir string
	// DestDir is the target plect data directory. Run refuses to promote
	// into one that already has a storage.db.
	DestDir string
	// DryRun builds and validates the temporary database and reports on it,
	// but never promotes it and never writes the legacy rejection marker.
	DryRun bool
}

// rejectionMarker is the legacy envelope every supported legacy binary
// refuses on startup and mutation: version 0 is deliberately outside the
// versions legacystate.ValidateVersion (and every earlier live loader it
// carries forward) ever accepted, and it holds no live session data.
const rejectionMarker = `{"version":0,"sessions":{}}`

// Run reads SourceDir, builds and validates a temporary SQLite database, and
// (unless DryRun) atomically promotes it to DestDir's storage.db and writes
// the legacy rejection marker at DestDir's state.json. It always returns a
// *Report describing what it found, even alongside a non-nil error, so a
// rejected import still explains itself.
func Run(ctx context.Context, opts Options) (*Report, error) {
	report := &Report{
		DBPath:     persistence.PathIn(opts.DestDir),
		MarkerPath: filepath.Join(opts.DestDir, "state.json"),
	}

	if opts.SourceDir == "" || opts.DestDir == "" {
		return report, fmt.Errorf("legacyimport: SourceDir and DestDir are both required")
	}

	sf, err := readLegacyState(opts.SourceDir)
	if err != nil {
		return report, err
	}

	if err := checkNotLocked(filepath.Join(opts.SourceDir, "state.json.lock")); err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}
	if err := checkNotLocked(filepath.Join(opts.SourceDir, "pending_delivery.json.lock")); err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}
	if err := checkDeliveryLocksNotHeld(opts.SourceDir); err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}

	eventsRoot := filepath.Join(opts.SourceDir, "events")
	sessionNames, err := ListLegacySessionDirs(eventsRoot)
	if err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}

	sessionLogs := make(map[string]*SessionLog, len(sessionNames))
	for _, name := range sessionNames {
		sl, err := ReadSessionDir(eventsRoot, name)
		if err != nil {
			return report, fmt.Errorf("legacyimport: %w", err)
		}
		if err := checkNotLocked(sl.lockPath); err != nil {
			return report, fmt.Errorf("legacyimport: %w", err)
		}
		report.UnknownFiles = append(report.UnknownFiles, sl.UnknownFiles...)
		sessionLogs[name] = sl
	}
	if len(report.UnknownFiles) > 0 {
		return report, fmt.Errorf("legacyimport: unrecognized files in the legacy events tree, not imported: %v", report.UnknownFiles)
	}

	allNames := unionSorted(sf.Sessions, sessionLogs)

	if !opts.DryRun {
		if _, err := os.Stat(report.DBPath); err == nil {
			return report, fmt.Errorf("legacyimport: %s already exists; import only targets a directory with no live database yet", report.DBPath)
		} else if !os.IsNotExist(err) {
			return report, fmt.Errorf("legacyimport: stat %s: %w", report.DBPath, err)
		}
	}

	// Unlike persistence.EnsureCurrent, Open does not create its own parent dir.
	if err := os.MkdirAll(opts.DestDir, 0o700); err != nil {
		return report, fmt.Errorf("legacyimport: create %s: %w", opts.DestDir, err)
	}

	tmpPath := report.DBPath + ".importing"
	if err := removeDatabaseFiles(tmpPath); err != nil {
		return report, fmt.Errorf("legacyimport: clear stale temp database: %w", err)
	}
	db, err := persistence.Open(tmpPath)
	if err != nil {
		return report, fmt.Errorf("legacyimport: open temporary database: %w", err)
	}
	defer func() {
		db.Close()
		// Idempotent regardless of outcome: a promoted run already renamed
		// the main file away, so this only clears its now-orphaned gate
		// sidecars; a dry run or a failure clears everything at tmpPath.
		removeDatabaseFiles(tmpPath)
	}()
	if err := db.Migrate(ctx); err != nil {
		return report, fmt.Errorf("legacyimport: migrate temporary database: %w", err)
	}

	importTime := time.Now().UTC()

	for key, pop := range sf.Populations {
		if pop == nil {
			continue
		}
		if err := db.UpdatePopulation(ctx, key, func(p *domain.PopulationState) error {
			p.Members = pop.Members
			return nil
		}); err != nil {
			return report, fmt.Errorf("legacyimport: import population %q: %w", key, err)
		}
		report.Populations++
		report.PopulationMembers += len(pop.Members)
	}

	for _, name := range allNames {
		s, hasState := sf.Sessions[name]
		if !hasState {
			// events/<name> exists with no state.json entry: a plain event
			// target (matching persistence.EnsureLiveSession's own
			// placeholder), never formally created via `plect create`.
			s = &domain.Session{Name: name, Status: contract.SessionStatusDown, CreatedAt: importTime, UpdatedAt: importTime}
			report.SessionsFromEventLogOnly++
		}

		id := ""
		if sl := sessionLogs[name]; sl != nil {
			id = sl.GenID
		}
		if id == "" {
			id = persistence.NewULID()
		}
		if err := db.ImportSession(ctx, id, s); err != nil {
			return report, fmt.Errorf("legacyimport: import session %q: %w", name, err)
		}
		report.Sessions++
	}

	for _, name := range allNames {
		sl := sessionLogs[name]
		if sl == nil {
			if lp, ok := sf.HeartbeatLogPositions[name]; ok && lp != 0 {
				return report, fmt.Errorf("legacyimport: session %q has tick_backoff.last_log_position %d but no event log directory", name, lp)
			}
			continue
		}
		for _, ev := range sl.Events {
			if _, err := db.AppendEvent(ctx, ev); err != nil {
				return report, fmt.Errorf("legacyimport: session %q: import event %q: %w", name, ev.ID, err)
			}
			report.Events++
		}
		report.InternalBackfilled += sl.InternalBackfilled

		for _, kind := range sortedKeys(sl.CursorOffsets) {
			offset := sl.CursorOffsets[kind]
			seq, ok := sl.Translate(offset)
			if !ok {
				return report, fmt.Errorf("legacyimport: session %q: %s cursor offset %d does not align with a complete log line", name, kind, offset)
			}
			if err := db.SetEventCursor(ctx, name, kind, seq); err != nil {
				return report, fmt.Errorf("legacyimport: session %q: set %s cursor: %w", name, kind, err)
			}
			report.Cursors++
		}

		if lp, ok := sf.HeartbeatLogPositions[name]; ok {
			seq, ok2 := sl.Translate(lp)
			if !ok2 {
				return report, fmt.Errorf("legacyimport: session %q: tick_backoff.last_log_position %d does not align with a complete log line", name, lp)
			}
			if err := db.SetEventCursor(ctx, name, "heartbeat", seq); err != nil {
				return report, fmt.Errorf("legacyimport: session %q: set heartbeat cursor: %w", name, err)
			}
			report.Cursors++
		}
	}

	// Second pass: resolve each state.json session's ParentSession and write
	// its tasks/channel health, now that every session row exists so
	// resolution never depends on map iteration order (see
	// persistence.ImportSession).
	for _, name := range allNames {
		s, hasState := sf.Sessions[name]
		if !hasState {
			continue
		}
		if err := db.PutSession(ctx, s); err != nil {
			return report, fmt.Errorf("legacyimport: session %q: resolve parent link: %w", name, err)
		}
	}

	for name, res := range sf.UpReservations {
		if err := db.ImportUpReservation(ctx, name, res); err != nil {
			return report, fmt.Errorf("legacyimport: import up-slot reservation %q: %w", name, err)
		}
		report.UpReservations++
	}

	if err := db.IntegrityCheck(ctx); err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}
	if err := validateCounts(ctx, db, allNames, sessionLogs, report); err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}

	if opts.DryRun {
		return report, nil
	}

	if err := db.Checkpoint(ctx); err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}
	if err := db.Close(); err != nil {
		return report, fmt.Errorf("legacyimport: close temporary database: %w", err)
	}

	// Marker before rename: the only failure left afterward is the rename
	// itself, leaving no storage.db — retryable — rather than a promoted
	// db sitting next to a legacy state.json no marker ever locked out.
	if err := atomicfile.Write(report.MarkerPath, []byte(rejectionMarker)); err != nil {
		return report, fmt.Errorf("legacyimport: write legacy rejection marker: %w", err)
	}
	if err := os.Rename(tmpPath, report.DBPath); err != nil {
		return report, fmt.Errorf("legacyimport: promote temporary database: %w", err)
	}
	report.Promoted = true // the deferred cleanup above still clears tmpPath's now-orphaned gate sidecars

	return report, nil
}

func readLegacyState(sourceDir string) (*legacystate.StateFile, error) {
	path := filepath.Join(sourceDir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("legacyimport: read %s: %w", path, err)
	}
	sf, err := legacystate.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("legacyimport: %w", err)
	}
	return sf, nil
}

func checkDeliveryLocksNotHeld(sourceDir string) error {
	dir := filepath.Join(sourceDir, "delivery-locks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := checkNotLocked(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// unionSorted returns every session name appearing in either the parsed
// state or the legacy event-log tree, sorted for deterministic import order
// (correctness does not depend on this order — see ImportSession — but a
// stable order keeps Run's error messages and any future progress log
// reproducible).
func unionSorted(sessions map[string]*domain.Session, logs map[string]*SessionLog) []string {
	seen := make(map[string]bool, len(sessions)+len(logs))
	var names []string
	for name := range sessions {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for name := range logs {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// removeDatabaseFiles removes path, its WAL-mode siblings (-wal, -shm), and
// persistence's own gate sidecars (.access.lock, .coordination.lock,
// .migration.json — see gate.go's newAccessGate), all of which are named
// after the temporary import path and would otherwise litter the
// destination directory once that path stops existing. Absence of any of
// them is not an error.
func removeDatabaseFiles(path string) error {
	suffixes := []string{"", "-wal", "-shm", ".access.lock", ".coordination.lock", ".migration.json"}
	for _, suffix := range suffixes {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// validateCounts re-reads the freshly built database and confirms every
// session and event this run intended to import is actually present, the
// second validation pass docs/design/sqlite-persistence.md's importer
// section calls for (IntegrityCheck is the first: SQLite's own structural
// check).
func validateCounts(ctx context.Context, db *persistence.DB, allNames []string, sessionLogs map[string]*SessionLog, report *Report) error {
	all, err := db.AllSessions(ctx)
	if err != nil {
		return fmt.Errorf("validate: list sessions: %w", err)
	}
	if len(all) != len(allNames) {
		return fmt.Errorf("validate: database has %d sessions, want %d", len(all), len(allNames))
	}
	for _, name := range allNames {
		if _, ok := all[name]; !ok {
			return fmt.Errorf("validate: session %q missing from the built database", name)
		}
		sl := sessionLogs[name]
		if sl == nil {
			continue
		}
		evs, _, err := db.ListEventsFrom(ctx, name, 0)
		if err != nil {
			return fmt.Errorf("validate: list events for %q: %w", name, err)
		}
		if len(evs) != len(sl.Events) {
			return fmt.Errorf("validate: session %q has %d events in the built database, want %d", name, len(evs), len(sl.Events))
		}
	}
	return nil
}
