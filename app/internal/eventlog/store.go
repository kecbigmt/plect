// Package eventlog is the durable, append-only per-session event log that backs
// the plect event bus, persisted via app/internal/persistence; session_name and Type stay opaque to it.
package eventlog

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/contracts/atomicfile"
	"github.com/kecbigmt/plecture/contracts/event"
)

// pollInterval is how often Follow checks the log for new records. A poll
// (vs fsnotify) avoids a third-party dependency; chat-volume traffic on a
// handful of sessions does not need sub-second latency.
const pollInterval = 500 * time.Millisecond

// Store manages per-session event logs rooted at <dir>/events.
type Store struct {
	dir    string
	root   string // <dir>/events: tombstone + chain-attempt sidecars only
	mu     sync.Mutex
	db     *persistence.DB
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
	return &Store{dir: dir, root: filepath.Join(dir, "events"), logger: slog.Default()}
}

// dbHandle lazily opens, migrates, and memoizes the database (mirroring state.Store.dbHandle).
func (s *Store) dbHandle() (*persistence.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, nil
	}
	db, err := persistence.EnsureCurrent(context.Background(), filepath.Join(s.dir, "store.db"))
	if err != nil {
		return nil, fmt.Errorf("eventlog: open database: %w", err)
	}
	s.db = db
	return db, nil
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

func (s *Store) tombstonePath(session string) string {
	return filepath.Join(s.sessionDir(session), "tombstone.json")
}
func (s *Store) chainAttemptsPath(session string) string {
	return filepath.Join(s.sessionDir(session), "chain_attempts.json")
}
func (s *Store) lockPath(session string) string { return filepath.Join(s.sessionDir(session), ".lock") }

// Append writes ev to its session's log and returns the stored event (with ID
// and Time filled in if absent), its sequence (the replay cursor), and next.
// A session with no current stream (never created through Create, or a
// notice about a resource whose admission never went through) gets one
// started here rather than rejecting the write: the guarantee Create's own
// NewStream protects is a fresh incarnation on every session create, not
// that every append target was already created.
func (s *Store) Append(ev event.Event) (stored event.Event, seq, next int64, err error) {
	if ev.SessionName == "" {
		return ev, 0, 0, fmt.Errorf("eventlog: session_name is required")
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.ID == "" {
		ev.ID = newULID(ev.Time)
	}

	db, err := s.dbHandle()
	if err != nil {
		return ev, 0, 0, err
	}
	ctx := context.Background()
	if id, gerr := db.EventStreamID(ctx, ev.SessionName); gerr != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: append: %w", gerr)
	} else if id == "" {
		if _, cerr := db.CreateEventStream(ctx, ev.SessionName); cerr != nil {
			return ev, 0, 0, fmt.Errorf("eventlog: append: %w", cerr)
		}
	}
	seq, err = db.AppendEvent(ctx, ev)
	if err != nil {
		return ev, 0, 0, fmt.Errorf("eventlog: append: %w", err)
	}
	return ev, seq, seq + 1, nil
}

// WriteTombstone durably persists a session's tombstone snapshot (atomic
// write + fsync) into its event log directory, so the snapshot survives
// `plect destroy` deleting the session's runtime state entry. data is an opaque
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
// a caller-supplied identity token (service.chainAttemptStreamID) so two incarnations never share a key even if a destroy races a leftover tick.
// previous is the value from just before this call, for RevertChainAttempt.
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
// under its exclusive per-session lock (unrelated to Append's own), writing
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

// List returns events from sequence `since` (inclusive) matching f, ascending, plus each event's sequence and the next read cursor.
func (s *Store) List(session string, since int64, f event.Filter) (evs []event.Event, seqs []int64, next int64, err error) {
	db, err := s.dbHandle()
	if err != nil {
		return nil, nil, 0, err
	}
	all, allSeqs, err := db.ListEventsFrom(context.Background(), session, max(since, 0))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("eventlog: list: %w", err)
	}
	next = since
	for i := range all {
		next = allSeqs[i] + 1
		if !f.Match(all[i]) {
			continue
		}
		evs = append(evs, all[i])
		seqs = append(seqs, allSeqs[i])
		if f.Limit > 0 && len(evs) >= f.Limit {
			break
		}
	}
	return evs, seqs, next, nil
}

// Tail returns up to the last `limit` events matching f for a session, in
// append order (oldest first); limit <= 0 returns all matches. It scans the log
// but retains only the last `limit` matching records in a ring, bounding memory
// and the caller's render size for long-lived sessions (the durable log can
// grow without bound and survives destroy). f selects which records the ring
// keeps — so `--order desc --limit N` returns the newest N *matching* events,
// not the newest N then filtered.
func (s *Store) Tail(session string, f event.Filter, limit int) ([]event.Event, error) {
	db, err := s.dbHandle()
	if err != nil {
		return nil, err
	}
	all, _, err := db.ListEventsFrom(context.Background(), session, 0)
	if err != nil {
		return nil, fmt.Errorf("eventlog: tail: %w", err)
	}
	var ring []event.Event
	for _, ev := range all {
		if !f.Match(ev) {
			continue
		}
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
	return ring, nil
}

// TailOffset returns the sequence at which a fresh stream should start so it
// replays only the last `n` records matching f. It scans the log keeping just
// the last `n` matching start offsets in a ring (bounded memory, unlike List
// which materializes every matching record), and returns 0 when there are <= n
// matches so the caller replays from the head. This is the cheap primitive the
// bus uses to scope a `?tail=N` replay over an unbounded log.
func (s *Store) TailOffset(session string, f event.Filter, n int) (int64, error) {
	if n <= 0 {
		return 0, nil
	}
	db, err := s.dbHandle()
	if err != nil {
		return 0, err
	}
	all, seqs, err := db.ListEventsFrom(context.Background(), session, 0)
	if err != nil {
		return 0, fmt.Errorf("eventlog: tail offset: %w", err)
	}
	ring := make([]int64, 0, n)
	for i, ev := range all {
		if !f.Match(ev) {
			continue
		}
		if len(ring) == n {
			copy(ring, ring[1:])
			ring[n-1] = seqs[i]
		} else {
			ring = append(ring, seqs[i])
		}
	}
	if len(ring) < n {
		return 0, nil // fewer than n matches → replay from the head
	}
	return ring[0], nil // sequence of the n-th-from-last matching record
}

// StreamID returns the current incarnation's stream id for session, or ""
// if none has been created yet. NewStream mints a fresh one on session
// create, so a cursor issued for a since-superseded incarnation resolves to
// a different id here and is detectable as stale rather than silently
// resolving into the wrong incarnation's log.
func (s *Store) StreamID(session string) (string, error) {
	db, err := s.dbHandle()
	if err != nil {
		return "", err
	}
	id, err := db.EventStreamID(context.Background(), session)
	if err != nil {
		return "", fmt.Errorf("eventlog: stream id: %w", err)
	}
	return id, nil
}

// NewStream mints a new event stream for session — one incarnation's log —
// and returns its id. It always creates, so callers own the decision of
// when a session name starts a new incarnation (a session create) versus
// resuming its current one (a down/up or --force-recreate).
func (s *Store) NewStream(session string) (string, error) {
	db, err := s.dbHandle()
	if err != nil {
		return "", err
	}
	id, err := db.CreateEventStream(context.Background(), session)
	if err != nil {
		return "", fmt.Errorf("eventlog: new stream: %w", err)
	}
	return id, nil
}

// Follow delivers events from `since`, then polls for new ones until ctx ends.
func (s *Store) Follow(ctx context.Context, session string, since int64, fn func(event.Event, int64)) error {
	cursor := since
	for {
		evs, seqs, next, err := s.List(session, cursor, event.Filter{})
		if err != nil {
			return err
		}
		for i := range evs {
			fn(evs[i], seqs[i])
		}
		cursor = next
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Sessions returns the names of every touched session, sorted for deterministic iteration. Missing database → empty, no error
// (nothing has been logged yet).
func (s *Store) Sessions() ([]string, error) {
	db, err := s.dbHandle()
	if err != nil {
		return nil, err
	}
	names, err := db.EventStreamSessions(context.Background())
	if err != nil {
		return nil, fmt.Errorf("eventlog: sessions: %w", err)
	}
	return names, nil
}

// ListAcross merges every event from the named sessions matching f into one
// slice sorted by event id (a ULID — lexicographic order is time order, and ids
// are globally unique, so this is a stable total order across sessions). It is
// the merge primitive behind the session-tree view (names = a session tree's
// root + descendants). f.Limit is ignored — paging is the caller's job (the
// service applies the ULID keyset cursor and page size). A per-session log
// that has rotated does not break the merge — ordering is by id, not by any
// per-session sequence.
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
// starts being followed automatically, tracking a per-session sequence so each record is delivered once, sorted by id so the merged order stays
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
	db, err := s.dbHandle()
	if err != nil {
		return false
	}
	// Treat a read error other than not-found as "exists" so a transient error
	// doesn't trigger an unwanted re-seed (which would re-read the log tail).
	has, err := db.HasEventCursor(context.Background(), session, consumer)
	if err != nil {
		return true
	}
	return has
}

// ReadCursor returns the committed offset for a named consumer (0 if none).
func (s *Store) ReadCursor(session, consumer string) (int64, error) {
	db, err := s.dbHandle()
	if err != nil {
		return 0, err
	}
	pos, err := db.EventCursor(context.Background(), session, consumer)
	if err != nil {
		return 0, fmt.Errorf("eventlog: read cursor: %w", err)
	}
	return pos, nil
}

// CommitCursor durably records a consumer's offset.
func (s *Store) CommitCursor(session, consumer string, seq int64) error {
	db, err := s.dbHandle()
	if err != nil {
		return err
	}
	if err := db.SetEventCursor(context.Background(), session, consumer, seq); err != nil {
		return fmt.Errorf("eventlog: commit cursor: %w", err)
	}
	return nil
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
