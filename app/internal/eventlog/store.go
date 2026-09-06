// Package eventlog is the durable, append-only per-session event log that backs
// the plect event bus. It is the source of truth: a session's log.jsonl is the
// authoritative queue; the bus server tails it to fan out to subscribers.
//
// The log is provider-agnostic: it treats session_name as an opaque string and
// only encodes it to a filesystem-safe directory name. It never interprets the
// name's structure (session, tags) or an event's Type.
package eventlog

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kecbigmt/plecture/contracts/atomicfile"
	"github.com/kecbigmt/plecture/contracts/event"
)

// pollInterval is how often Follow checks the log for new records. A poll
// (vs fsnotify) avoids a third-party dependency; chat-volume traffic on a
// handful of sessions does not need sub-second latency.
const pollInterval = 500 * time.Millisecond

// Store manages per-session event logs rooted at <dir>/events.
type Store struct {
	root   string
	mu     sync.Mutex // serializes this process's appends across sessions
	logger *slog.Logger
}

// Root returns the events directory this store reads/writes (for diagnostics —
// e.g. confirming the resident process and writers resolve the same log tree).
func (s *Store) Root() string { return s.root }

// NewStore creates a Store. If dir is empty it defaults to ~/.local/share/plect
// (honoring XDG_DATA_HOME), matching state.NewStore so both live side by side.
func NewStore(dir string) *Store {
	if dir == "" {
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			home, _ := os.UserHomeDir()
			dataHome = filepath.Join(home, ".local", "share")
		}
		dir = filepath.Join(dataHome, "plect")
	}
	return &Store{root: filepath.Join(dir, "events"), logger: slog.Default()}
}

// sessionDir returns the directory holding a session's log, encoding the opaque
// session name to a single filesystem-safe path segment.
func (s *Store) sessionDir(session string) string {
	return filepath.Join(s.root, encodeSession(session))
}

// encodeSession maps the opaque session name to one filesystem-safe path
// segment (e.g. a session name containing "/" is percent-escaped). Each
// log record also carries session_name, so this need not be reversed.
func encodeSession(session string) string {
	return url.PathEscape(session)
}

func (s *Store) logPath(session string) string {
	return filepath.Join(s.sessionDir(session), "log.jsonl")
}
func (s *Store) tombstonePath(session string) string {
	return filepath.Join(s.sessionDir(session), "tombstone.json")
}
func (s *Store) chainAttemptsPath(session string) string {
	return filepath.Join(s.sessionDir(session), "chain_attempts.json")
}
func (s *Store) lockPath(session string) string { return filepath.Join(s.sessionDir(session), ".lock") }
func (s *Store) genPath(session string) string  { return filepath.Join(s.sessionDir(session), ".gen") }
func (s *Store) cursorPath(session, consumer string) string {
	return filepath.Join(s.sessionDir(session), ".cursor."+consumer)
}

// Append writes ev to its session's log and returns the stored event (with ID
// and Time filled in if absent), the byte offset where the record starts (its
// replay cursor), and the offset just past it.
func (s *Store) Append(ev event.Event) (stored event.Event, off, next int64, err error) {
	if ev.SessionName == "" {
		return ev, 0, 0, fmt.Errorf("eventlog: session_name is required")
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.ID == "" {
		ev.ID = newULID(ev.Time)
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: marshal: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.sessionDir(ev.SessionName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: mkdir: %w", err)
	}

	unlock, err := flock(s.lockPath(ev.SessionName), syscall.LOCK_EX)
	if err != nil {
		return ev, 0, 0, err
	}
	defer unlock()

	if err := s.ensureGen(ev.SessionName); err != nil {
		return ev, 0, 0, err
	}

	f, err := os.OpenFile(s.logPath(ev.SessionName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: open: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: stat: %w", err)
	}
	off = info.Size() // start byte of this record, stable under LOCK_EX

	if _, err := f.Write(line); err != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: fsync: %w", err)
	}
	return ev, off, off + int64(len(line)), nil
}

// WriteTombstone durably persists a session's tombstone snapshot (atomic
// write + fsync) into its event log directory, so the snapshot survives
// `plect destroy` deleting the session's state.json entry. data is an opaque
// blob (the caller owns its schema — this package stays provider-agnostic);
// a pre-existing tombstone is overwritten.
func (s *Store) WriteTombstone(session string, data []byte) error {
	dir := s.sessionDir(session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("eventlog: mkdir: %w", err)
	}
	return atomicfile.Write(s.tombstonePath(session), data)
}

// ReadTombstone returns a session's tombstone blob and whether one exists.
func (s *Store) ReadTombstone(session string) (data []byte, ok bool, err error) {
	data, err = os.ReadFile(s.tombstonePath(session))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

// SwapChainAttempt atomically compares-and-sets a plect.chain.attempt
// cap-refusal streak marker (see service.chainAttemptFingerprint), scoped by
// generation (a session's CreatedAt) so a session destroyed and recreated
// under the same name never inherits — or, via some stale in-flight tick
// still racing the destroy, resurrects — its predecessor's marker: the two
// generations never share a key, regardless of how the destroy and any
// leftover tick against the old generation interleave. previous is the value
// from just before this call, for RevertChainAttempt.
func (s *Store) SwapChainAttempt(session, instance, chainID, generation, newFingerprint string) (previous string, won bool, err error) {
	key := chainAttemptKey(instance, chainID, generation)
	err = s.withChainAttemptsLocked(session, func(attempts map[string]string) bool {
		previous = attempts[key]
		if previous == newFingerprint {
			return false
		}
		won = true
		setChainAttempt(attempts, key, newFingerprint)
		return true
	})
	return previous, won, err
}

// RevertChainAttempt compensates a SwapChainAttempt win whose side effect
// failed, restoring previous — but only if the marker still holds exactly
// claimed. Restoring unconditionally would erase a legitimate later
// transition (a concurrent tick's own, newer streak) if one has since won;
// finding the marker already past claimed means that already happened, so
// there is nothing here for this caller to compensate.
func (s *Store) RevertChainAttempt(session, instance, chainID, generation, claimed, previous string) (reverted bool, err error) {
	key := chainAttemptKey(instance, chainID, generation)
	err = s.withChainAttemptsLocked(session, func(attempts map[string]string) bool {
		if attempts[key] != claimed {
			return false
		}
		reverted = true
		setChainAttempt(attempts, key, previous)
		return true
	})
	return reverted, err
}

// ClearChainAttempts removes every chain-attempt marker for session,
// including any left by earlier generations — a hygiene sweep, not a
// correctness requirement now that SwapChainAttempt/RevertChainAttempt scope
// each generation to its own key.
func (s *Store) ClearChainAttempts(session string) error {
	if err := os.Remove(s.chainAttemptsPath(session)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("eventlog: chain attempts: remove: %w", err)
	}
	return nil
}

func chainAttemptKey(instance, chainID, generation string) string {
	return instance + "\x00" + chainID + "\x00" + generation
}

func setChainAttempt(attempts map[string]string, key, value string) {
	if value == "" {
		delete(attempts, key)
		return
	}
	attempts[key] = value
}

// withChainAttemptsLocked runs fn against session's chain-attempt markers
// under its exclusive per-session lock (the same one Append uses), writing
// the result back only when fn reports a change — the shared plumbing
// SwapChainAttempt and RevertChainAttempt each apply their own compare
// logic through.
func (s *Store) withChainAttemptsLocked(session string, fn func(attempts map[string]string) (changed bool)) error {
	dir := s.sessionDir(session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("eventlog: mkdir: %w", err)
	}
	unlock, err := flock(s.lockPath(session), syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()

	attempts, err := s.readChainAttemptsLocked(session)
	if err != nil {
		return err
	}
	if !fn(attempts) {
		return nil
	}
	data, merr := json.Marshal(attempts)
	if merr != nil {
		return fmt.Errorf("eventlog: chain attempts: marshal: %w", merr)
	}
	return atomicfile.Write(s.chainAttemptsPath(session), data)
}

func (s *Store) readChainAttemptsLocked(session string) (map[string]string, error) {
	data, err := os.ReadFile(s.chainAttemptsPath(session))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("eventlog: chain attempts: read: %w", err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("eventlog: chain attempts: unmarshal: %w", err)
	}
	return out, nil
}

// List returns events from byte offset `since` (inclusive) that match f, in
// append order, plus each event's offset and the next read cursor (the offset
// just past the last complete record read). A torn trailing line (an append in
// flight) is skipped. f.Limit>0 stops after that many matching events.
//
// A line that fails to parse as JSON (on-disk corruption; Append never writes
// one) is logged via s.logger and its bytes excluded from evs, but next still
// advances past it: a cursor that never advances past can't be recovered by
// retrying the same offset, and would wedge every event after it behind one
// bad line forever.
func (s *Store) List(session string, since int64, f event.Filter) (evs []event.Event, offs []int64, next int64, err error) {
	unlock, err := flock(s.lockPath(session), syscall.LOCK_SH)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, since, nil // no log yet
		}
		return nil, nil, 0, err
	}
	defer unlock()

	file, err := os.Open(s.logPath(session))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, since, nil
		}
		return nil, nil, 0, err
	}
	defer file.Close()

	if _, err := file.Seek(since, io.SeekStart); err != nil {
		return nil, nil, 0, err
	}

	r := bufio.NewReader(file)
	pos := since
	next = since
	for {
		lineBytes, rerr := r.ReadBytes('\n')
		if len(lineBytes) > 0 && lineBytes[len(lineBytes)-1] == '\n' {
			// complete record
			start := pos
			pos += int64(len(lineBytes))
			next = pos
			var ev event.Event
			if uerr := json.Unmarshal(lineBytes[:len(lineBytes)-1], &ev); uerr != nil {
				s.logMalformed(session, start, uerr)
			} else if f.Match(ev) {
				evs = append(evs, ev)
				offs = append(offs, start)
				if f.Limit > 0 && len(evs) >= f.Limit {
					return evs, offs, next, nil
				}
			}
		}
		if rerr == io.EOF {
			return evs, offs, next, nil // trailing partial line (if any) dropped
		}
		if rerr != nil {
			return evs, offs, next, rerr
		}
	}
}

// Tail returns up to the last `limit` events matching f for a session, in
// append order (oldest first); limit <= 0 returns all matches. It scans the log
// but retains only the last `limit` matching records in a ring, bounding memory
// and the caller's render size for long-lived sessions (the durable log can
// grow without bound and survives destroy). f selects which records the ring
// keeps — so `--order desc --limit N` returns the newest N *matching* events,
// not the newest N then filtered. A torn trailing line is skipped, as in List.
func (s *Store) Tail(session string, f event.Filter, limit int) ([]event.Event, error) {
	unlock, err := flock(s.lockPath(session), syscall.LOCK_SH)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no log yet
		}
		return nil, err
	}
	defer unlock()

	file, err := os.Open(s.logPath(session))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	r := bufio.NewReader(file)
	var ring []event.Event
	var pos int64
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			start := pos
			pos += int64(len(line))
			var ev event.Event
			if uerr := json.Unmarshal(line[:len(line)-1], &ev); uerr != nil {
				s.logMalformed(session, start, uerr)
			} else if f.Match(ev) {
				switch {
				case limit <= 0:
					ring = append(ring, ev)
				case len(ring) == limit:
					copy(ring, ring[1:])
					ring[limit-1] = ev
				default:
					ring = append(ring, ev)
				}
			}
		}
		if rerr == io.EOF {
			return ring, nil
		}
		if rerr != nil {
			return ring, rerr
		}
	}
}

// TailOffset returns the byte offset at which a fresh stream should start so it
// replays only the last `n` records matching f. It scans the log keeping just
// the last `n` matching start offsets in a ring (bounded memory, unlike List
// which materializes every matching record), and returns 0 when there are <= n
// matches so the caller replays from the head. This is the cheap primitive the
// bus uses to scope a `?tail=N` replay over an unbounded log.
func (s *Store) TailOffset(session string, f event.Filter, n int) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	unlock, err := flock(s.lockPath(session), syscall.LOCK_SH)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // no log yet
		}
		return 0, err
	}
	defer unlock()

	file, err := os.Open(s.logPath(session))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer file.Close()

	r := bufio.NewReader(file)
	ring := make([]int64, 0, n)
	var pos int64
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			start := pos
			pos += int64(len(line))
			var ev event.Event
			if uerr := json.Unmarshal(line[:len(line)-1], &ev); uerr != nil {
				s.logMalformed(session, start, uerr)
			} else if f.Match(ev) {
				if len(ring) == n {
					copy(ring, ring[1:])
					ring[n-1] = start
				} else {
					ring = append(ring, start)
				}
			}
		}
		if rerr == io.EOF {
			if len(ring) < n {
				return 0, nil // fewer than n matches → replay from the head
			}
			return ring[0], nil // start of the n-th-from-last matching record
		}
		if rerr != nil {
			return 0, rerr
		}
	}
}

// Gen returns the log's generation id, or "" if the log has none yet (no event
// appended, or a legacy log created before generation ids). The id is assigned
// once when the log is first created and is stable across appends; it changes
// only if the log is rotated or compacted, which lets a stale opaque cursor be
// detected instead of silently resolving to a shifted record. It reads under
// LOCK_SH, the same discipline as List/Tail, so it never races a concurrent
// ensureGen write (which holds LOCK_EX).
func (s *Store) Gen(session string) (string, error) {
	unlock, err := flock(s.lockPath(session), syscall.LOCK_SH)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // no session dir yet
		}
		return "", err
	}
	defer unlock()

	data, err := os.ReadFile(s.genPath(session))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// ensureGen writes the session's generation id if absent. It must be called
// under LOCK_EX (from Append): the exclusive lock excludes both concurrent
// appenders and LOCK_SH readers, so a plain write cannot be observed torn.
func (s *Store) ensureGen(session string) error {
	path := s.genPath(session)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("eventlog: stat gen: %w", err)
	}
	if err := os.WriteFile(path, []byte(newULID(time.Now().UTC())), 0o644); err != nil {
		return fmt.Errorf("eventlog: write gen: %w", err)
	}
	return nil
}

// Follow delivers events from `since`, then polls for new ones until ctx ends.
func (s *Store) Follow(ctx context.Context, session string, since int64, fn func(event.Event, int64)) error {
	cursor := since
	for {
		evs, offs, next, err := s.List(session, cursor, event.Filter{})
		if err != nil {
			return err
		}
		for i := range evs {
			fn(evs[i], offs[i])
		}
		cursor = next
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Sessions returns the names of every session that has a log directory, decoding
// each dir name back to its opaque session name (the inverse of encodeSession).
// Returned sorted for deterministic iteration. Missing root → empty, no error
// (nothing has been logged yet).
func (s *Store) Sessions() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, derr := url.PathUnescape(e.Name())
		if derr != nil {
			continue // not a session dir we wrote; skip rather than fail the sweep
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// ListAcross merges every event from the named sessions matching f into one
// slice sorted by event id (a ULID — lexicographic order is time order, and ids
// are globally unique, so this is a stable total order across sessions). It is
// the merge primitive behind the session-tree view (names = a session tree's
// root + descendants). f.Limit is ignored — paging is the caller's job (the
// service applies the ULID keyset cursor and page size). A per-session log
// that has rotated does not break the merge — ordering is by id, not by any
// per-session byte offset.
func (s *Store) ListAcross(names []string, f event.Filter) ([]event.Event, error) {
	lf := f
	lf.Limit = 0
	var all []event.Event
	for _, name := range names {
		evs, _, _, lerr := s.List(name, 0, lf)
		if lerr != nil {
			return nil, lerr
		}
		all = append(all, evs...)
	}
	slices.SortFunc(all, func(a, b event.Event) int { return strings.Compare(a.ID, b.ID) })
	return all, nil
}

// FollowAcross delivers events as they land across a dynamic set of sessions,
// then keeps polling until ctx ends. It re-resolves membership each tick via
// namesFn so a session that joins later — a freshly spawned subtree child —
// starts being followed automatically, and tracks
// a per-session byte offset so each record is delivered once. Within a tick new
// records are sorted by id before delivery so the merged order stays
// chronological; across ticks ordering is monotonic at poll granularity.
func (s *Store) FollowAcross(ctx context.Context, namesFn func() ([]string, error), f event.Filter, fn func(event.Event)) error {
	lf := f
	lf.Limit = 0
	offsets := map[string]int64{}
	for {
		names, err := namesFn()
		if err != nil {
			return err
		}
		var batch []event.Event
		for _, name := range names {
			evs, _, next, lerr := s.List(name, offsets[name], lf)
			if lerr != nil {
				return lerr
			}
			batch = append(batch, evs...)
			offsets[name] = next
		}
		slices.SortFunc(batch, func(a, b event.Event) int { return strings.Compare(a.ID, b.ID) })
		for i := range batch {
			fn(batch[i])
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// HasCursor reports whether a consumer has ever committed an offset. It lets a
// consumer distinguish "never started" from a committed offset of 0 (which
// ReadCursor cannot), so a first start can begin at the live tail instead of
// replaying the whole log.
func (s *Store) HasCursor(session, consumer string) bool {
	// Treat a stat error other than not-exist as "exists" so a transient error
	// doesn't trigger an unwanted re-seed (which would re-read the log tail).
	_, err := os.Stat(s.cursorPath(session, consumer))
	return !os.IsNotExist(err)
}

// ReadCursor returns the committed offset for a named consumer (0 if none).
func (s *Store) ReadCursor(session, consumer string) (int64, error) {
	data, err := os.ReadFile(s.cursorPath(session, consumer))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("eventlog: bad cursor %q: %w", consumer, err)
	}
	return n, nil
}

// CommitCursor durably records a consumer's offset (atomic write + fsync).
func (s *Store) CommitCursor(session, consumer string, off int64) error {
	dir := s.sessionDir(session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicfile.Write(s.cursorPath(session, consumer), []byte(strconv.FormatInt(off, 10)))
}

// logMalformed reports a log line that failed to parse as JSON, so the
// corruption is observable (logs/monitoring) instead of a silent skip.
func (s *Store) logMalformed(session string, offset int64, err error) {
	s.logger.Warn("eventlog: skipping malformed record", "session", session, "offset", offset, "error", err)
}

// flock opens (creating) the lock file and takes the given flock mode, returning
// an unlock func. The session dir must already exist for LOCK_EX callers.
//
// The descriptor is opened O_RDWR even for a LOCK_SH caller: this one helper
// is shared with LOCK_EX, and the Linux NFS client enforces that LOCK_EX
// requires a writable descriptor, returning EBADF for an O_RDONLY one, even
// though local filesystems tolerate it. The descriptor is never read or
// written to; only its lock is used.
func flock(path string, how int) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("eventlog: flock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// entropy is a monotonic ULID source: for two ids minted in the same
// millisecond it strictly increases the random component, so ids sort in
// mint order, not by a random tiebreak. This matters for the cross-session
// (subtree) view, whose merge and keyset cursor order events by id — without
// monotonicity, two events appended in the same millisecond (even in different
// sessions, as long as one process writes them) could sort either way. It is
// stateful and not safe for concurrent use, so every mint goes through entMu.
// Across processes, same-millisecond ids still fall back to a random order;
// that is rare and acceptable (event timestamps are far enough apart in
// practice), and ids stay globally unique either way.
var (
	entMu   sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

func newULID(t time.Time) string {
	entMu.Lock()
	defer entMu.Unlock()
	id, err := ulid.New(ulid.Timestamp(t), entropy)
	if err != nil {
		// crypto/rand essentially never fails; fall back to a time-only id.
		return fmt.Sprintf("t%020d", t.UnixNano())
	}
	return id.String()
}
