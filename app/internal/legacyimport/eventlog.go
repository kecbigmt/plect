package legacyimport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kecbigmt/plecture/contracts/event"
)

// legacyConsumerToCursorKind maps a pre-cutover `.cursor.<consumer>` file's
// consumer name to the destination event_cursors.kind it imports as. These
// are frozen historical facts (the consumer literals as they were the
// commit before the SQLite event-store cutover), not today's kind
// vocabulary, which dispatcher.go/reactor.go coincidentally already share
// ("delivery"/"tick") — see docs/design/sqlite-persistence.md's importer
// inventory table.
var legacyConsumerToCursorKind = map[string]string{
	"dispatcher":   "delivery",
	"tick-reactor": "tick",
}

// SessionLog is one legacy events/<escaped-session> directory's validated
// content: its decoded log, its generation id (if any), and its consumer
// cursor positions (still raw legacy byte offsets — see Translate).
type SessionLog struct {
	Name  string
	GenID string
	// Events is the decoded, in-order log; Events[i]'s destination sequence
	// is i+1 (AppendEvent assigns sequences the same way on import as on any
	// other append, since events are imported in original order).
	Events []event.Event
	// InternalBackfilled counts events with no direction, which import as
	// "internal" (see docs/design/sqlite-persistence.md's inventory table).
	InternalBackfilled int
	// CursorOffsets is each recognized `.cursor.<consumer>` file's raw
	// legacy byte offset, keyed by destination event_cursors.kind
	// ("delivery"/"tick"). Translate resolves these (and a session's
	// tick_backoff.last_log_position "heartbeat" offset) against
	// lineEndOffsets.
	CursorOffsets map[string]int64
	// UnknownFiles are regular files in this session directory this package
	// does not recognize; a non-empty slice fails validation rather than
	// silently discarding a prospective durable sidecar.
	UnknownFiles []string
	// lockPath is checked by the caller (checkNotLocked), not here: reading
	// a session's files never itself requires holding its lock.
	lockPath string

	lineEndOffsets []int64 // end byte offset of Events[i]'s source line, ascending
}

// Translate maps a legacy byte offset (a `.cursor.<consumer>` value, or a
// session's tick_backoff.last_log_position) to the destination
// event_cursors.next_sequence it denotes: 0 means "nothing consumed yet"
// (next_sequence 1); any other valid value is the end boundary of some
// complete source line, meaning "next_sequence is one past that line's own
// sequence". An offset that is negative, beyond the log, mid-line, or inside
// a discarded trailing partial line is invalid.
func (l *SessionLog) Translate(offset int64) (nextSequence int64, ok bool) {
	if offset == 0 {
		return 1, true
	}
	if offset < 0 {
		return 0, false
	}
	// lineEndOffsets is small per session in practice and only consulted a
	// handful of times per session (at most one per recognized cursor kind
	// plus heartbeat); a linear scan keeps this package independent of a
	// sorted-search invariant a future change could silently break.
	for i, end := range l.lineEndOffsets {
		if end == offset {
			return int64(i) + 2, true
		}
	}
	return 0, false
}

// encodeSessionDir mirrors the pre-cutover eventlog.Store's encodeSession:
// the opaque session name percent-escaped to one filesystem-safe segment.
func encodeSessionDir(session string) string {
	return url.PathEscape(session)
}

// ListLegacySessionDirs returns every session name with a directory under
// root (a legacy data directory's "events" subdirectory), sorted for
// deterministic import order. A missing root is not an error: a backup with
// no event history yet imports zero sessions from it.
func ListLegacySessionDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("legacyimport: read events dir %q: %w", root, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, derr := url.PathUnescape(e.Name())
		if derr != nil {
			return nil, fmt.Errorf("legacyimport: events dir entry %q is not a valid encoded session name: %w", e.Name(), derr)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ReadSessionDir reads and validates one legacy session's event directory.
func ReadSessionDir(eventsRoot, name string) (*SessionLog, error) {
	dir := filepath.Join(eventsRoot, encodeSessionDir(name))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("legacyimport: read session dir %q: %w", dir, err)
	}

	sl := &SessionLog{
		Name:          name,
		CursorOffsets: map[string]int64{},
		lockPath:      filepath.Join(dir, ".lock"),
	}

	var haveLog bool
	for _, e := range entries {
		if e.IsDir() {
			sl.UnknownFiles = append(sl.UnknownFiles, filepath.Join(dir, e.Name()))
			continue
		}
		switch {
		case e.Name() == "log.jsonl":
			haveLog = true
		case e.Name() == ".gen", e.Name() == ".lock", e.Name() == "tombstone.json", e.Name() == "chain_attempts.json":
			// Recognized, but not imported into SQLite: .gen is read below,
			// .lock is validated by checkNotLocked, tombstone.json and
			// chain_attempts.json stay file-based after cutover (see
			// package doc).
		case strings.HasPrefix(e.Name(), ".cursor."):
			consumer := strings.TrimPrefix(e.Name(), ".cursor.")
			kind, known := legacyConsumerToCursorKind[consumer]
			if !known {
				sl.UnknownFiles = append(sl.UnknownFiles, filepath.Join(dir, e.Name()))
				continue
			}
			offset, rerr := readCursorFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				return nil, fmt.Errorf("legacyimport: session %q: %w", name, rerr)
			}
			sl.CursorOffsets[kind] = offset
		default:
			sl.UnknownFiles = append(sl.UnknownFiles, filepath.Join(dir, e.Name()))
		}
	}

	genID, err := readGenFile(filepath.Join(dir, ".gen"))
	if err != nil {
		return nil, fmt.Errorf("legacyimport: session %q: %w", name, err)
	}
	sl.GenID = genID

	if haveLog {
		events, ends, internalBackfilled, err := readLegacyLog(filepath.Join(dir, "log.jsonl"), name)
		if err != nil {
			return nil, fmt.Errorf("legacyimport: session %q: %w", name, err)
		}
		sl.Events = events
		sl.lineEndOffsets = ends
		sl.InternalBackfilled = internalBackfilled
	}

	return sl, nil
}

func readGenFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read .gen: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func readCursorFile(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: not a decimal offset: %w", filepath.Base(path), err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s: negative offset %d", filepath.Base(path), n)
	}
	return n, nil
}

// readLegacyLog decodes log.jsonl's complete lines in byte order, exactly as
// the pre-cutover eventlog.Store.List read them, except that a malformed
// complete line, a session mismatch, a missing id, or a duplicate id fails
// the import instead of being logged and skipped — the live reader's
// tolerance existed to keep serving a still-running store through
// corruption; a one-time import has no such excuse and must not silently
// promote a database missing data. A trailing partial line (a torn
// in-flight append) is still discarded silently, matching the live reader.
func readLegacyLog(path, sessionName string) (events []event.Event, lineEndOffsets []int64, internalBackfilled int, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, 0, nil
		}
		return nil, nil, 0, fmt.Errorf("open log.jsonl: %w", err)
	}
	defer f.Close()

	seen := map[string]bool{}
	r := bufio.NewReader(f)
	var pos int64
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			start := pos
			end := pos + int64(len(line))
			pos = end

			var ev event.Event
			if uerr := json.Unmarshal(line[:len(line)-1], &ev); uerr != nil {
				return nil, nil, 0, fmt.Errorf("log.jsonl: malformed record at offset %d: %w", start, uerr)
			}
			if ev.ID == "" {
				return nil, nil, 0, fmt.Errorf("log.jsonl: record at offset %d has no id", start)
			}
			if seen[ev.ID] {
				return nil, nil, 0, fmt.Errorf("log.jsonl: duplicate event id %q at offset %d", ev.ID, start)
			}
			seen[ev.ID] = true
			if ev.SessionName != sessionName {
				return nil, nil, 0, fmt.Errorf("log.jsonl: record %q at offset %d has session_name %q, want %q", ev.ID, start, ev.SessionName, sessionName)
			}
			if ev.Direction == "" {
				ev.Direction = event.Internal
				internalBackfilled++
			}
			events = append(events, ev)
			lineEndOffsets = append(lineEndOffsets, end)
		}
		if rerr == io.EOF {
			return events, lineEndOffsets, internalBackfilled, nil
		}
		if rerr != nil {
			return nil, nil, 0, fmt.Errorf("read log.jsonl: %w", rerr)
		}
	}
}
