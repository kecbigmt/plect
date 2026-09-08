package legacyimport

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/legacystate"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
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
	// TmpDir is the directory Run builds its scratch database under before
	// copying it into DestDir (empty defaults to os.TempDir()).
	TmpDir string
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

	// Only a state.json session's own event log is read: an events/-only
	// directory carries no parent, workflow, resource, inputs, or lifecycle
	// facts to build a session row from, so it is skipped and only counted.
	sessionLogs := make(map[string]*SessionLog, len(sf.Sessions))
	for _, name := range sessionNames {
		if _, hasState := sf.Sessions[name]; !hasState {
			report.SkippedEventOnly++
			continue
		}
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

	allNames := sortedSessionNames(sf.Sessions)

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

	scratchDir, err := newScratchDir(opts.TmpDir)
	if err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}
	defer os.RemoveAll(scratchDir)
	scratchDBPath := filepath.Join(scratchDir, "storage.db")

	db, err := persistence.Open(scratchDBPath)
	if err != nil {
		return report, fmt.Errorf("legacyimport: open temporary database: %w", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return report, fmt.Errorf("legacyimport: migrate temporary database: %w", err)
	}

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
		s := sf.Sessions[name]
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

	// Second pass: resolve each session's ParentSession and write its
	// tasks/channel health, now that every session row exists so resolution
	// never depends on map iteration order (see persistence.ImportSession).
	for _, name := range allNames {
		if err := db.PutSession(ctx, sf.Sessions[name]); err != nil {
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

	// Close releases scratchDBPath so the copy below sees a stable file.
	if err := db.Checkpoint(ctx); err != nil {
		return report, fmt.Errorf("legacyimport: %w", err)
	}
	if err := db.Close(); err != nil {
		return report, fmt.Errorf("legacyimport: close temporary database: %w", err)
	}

	destTmpPath := report.DBPath + ".importing"
	if err := os.Remove(destTmpPath); err != nil && !os.IsNotExist(err) {
		return report, fmt.Errorf("legacyimport: clear stale temp database: %w", err)
	}
	if err := copyFileWithFsync(scratchDBPath, destTmpPath); err != nil {
		return report, fmt.Errorf("legacyimport: copy built database into %s: %w", opts.DestDir, err)
	}
	// Idempotent regardless of outcome: a promoted run already renamed this
	// away, so this only clears a leftover from a failure below.
	defer os.Remove(destTmpPath)

	// Marker before rename: the only failure left afterward is the rename
	// itself, leaving no storage.db — retryable — rather than a promoted
	// db sitting next to a legacy state.json no marker ever locked out.
	if err := atomicfile.Write(report.MarkerPath, []byte(rejectionMarker)); err != nil {
		return report, fmt.Errorf("legacyimport: write legacy rejection marker: %w", err)
	}
	if err := os.Rename(destTmpPath, report.DBPath); err != nil {
		return report, fmt.Errorf("legacyimport: promote temporary database: %w", err)
	}
	report.Promoted = true

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

// sortedSessionNames returns every state.json session name, sorted for
// deterministic import order (correctness does not depend on this order —
// see ImportSession — but a stable order keeps Run's error messages and any
// future progress log reproducible).
func sortedSessionNames(sessions map[string]*domain.Session) []string {
	names := make([]string, 0, len(sessions))
	for name := range sessions {
		names = append(names, name)
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

// newScratchDir creates a fresh directory to build the scratch database
// under: tmpDirOverride if non-empty, else os.TempDir() -- never a path
// derived from DestDir, so DestDir stays untouched until the final copy.
func newScratchDir(tmpDirOverride string) (string, error) {
	base := tmpDirOverride
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "plect-storage-import-*")
	if err != nil {
		return "", fmt.Errorf("create scratch directory under %s: %w", base, err)
	}
	return dir, nil
}

// copyFileWithFsync copies src to dst, fsyncing dst before close. O_EXCL
// rejects an already-existing dst rather than silently overwriting it.
func copyFileWithFsync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("fsync %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("close %s: %w", dst, err)
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
