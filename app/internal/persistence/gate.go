package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/kecbigmt/plecture/contracts/atomicfile"
)

// migrationWait bounds how long a process waits for another process's
// migration-related lock to clear before it gives up and refuses, per
// docs/design/sqlite-persistence.md's migration access gate. It is a fixed
// constant, not a configuration value: no flag or config key adjusts it.
const migrationWait = 30 * time.Second

// coordinationPollInterval paces the retry loop behind migrationWait. It is
// short enough that a process notices a cleared lock quickly without
// busy-spinning the filesystem.
const coordinationPollInterval = 100 * time.Millisecond

// accessGate implements the two advisory lock files the design places next
// to the database file, plus the diagnostics-only migration marker.
//
//   - accessLockPath ("<db>.access.lock") is the data-access gate. A normal
//     query or transaction holds it shared for its duration; a migration
//     holds it exclusive for the whole migration.
//   - coordinationLockPath ("<db>.coordination.lock") serializes migration
//     intent. A normal access briefly holds it shared before taking the
//     access gate; a migration holds it exclusive from the moment it commits
//     to migrating until the migration finishes or fails.
//   - markerPath ("<db>.migration.json") records diagnostics for whichever
//     process currently (or most recently) holds the coordination lock
//     exclusively. It is never consulted to decide whether a migration is in
//     progress — only the coordination lock's own state answers that,
//     because a marker can outlive the process that wrote it (a crash
//     releases the lock but leaves the file), and treating a stale file as
//     "in progress" would wedge every later access. The marker is read only
//     to make an already-timed-out wait's error message actionable.
type accessGate struct {
	accessLockPath       string
	coordinationLockPath string
	markerPath           string
}

func newAccessGate(dbPath string) *accessGate {
	return &accessGate{
		accessLockPath:       dbPath + ".access.lock",
		coordinationLockPath: dbPath + ".coordination.lock",
		markerPath:           dbPath + ".migration.json",
	}
}

// accessShared is held for the duration of one normal query or explicit
// transaction. It blocks only while a migration holds accessExclusive. It
// is unexported: every normal access must go through enterShared instead,
// which performs the coordination probe first — calling accessShared alone
// would let a new operation race a migrator that has already recorded
// intent (announced by holding the coordination lock exclusively) but has
// not yet reached accessExclusive, because flock grants a new shared
// request the instant nothing currently holds the lock exclusively,
// regardless of an exclusive waiter queued behind it.
func (g *accessGate) accessShared() (func(), error) {
	unlock, err := flockPath(g.accessLockPath, syscall.LOCK_SH)
	if err != nil {
		return nil, fmt.Errorf("persistence: acquire access lock: %w", err)
	}
	return unlock, nil
}

// enterShared is the full normal-access protocol every read, write
// transaction, and even the initial connection Open must go through: it
// holds the coordination lock shared across the access lock acquisition,
// bounded by migrationWait, rather than releasing the coordination lock
// before requesting the access lock. Release-then-acquire would leave a gap
// between the two calls in which a migrator could take the coordination
// lock exclusively — recording intent — while this operation is still on
// its way in, which would defeat the exclusion the coordination lock exists
// to provide. Once accessShared succeeds, the coordination lock is
// released; this operation is now the kind accessExclusive itself waits
// out, per design.
func (g *accessGate) enterShared(ctx context.Context) (func(), error) {
	unlockCoord, err := g.acquireCoordination(ctx, syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	unlockAccess, err := g.accessShared()
	if err != nil {
		unlockCoord()
		return nil, err
	}
	unlockCoord()
	return unlockAccess, nil
}

// accessExclusive is held for the duration of a migration. It waits for
// every already-in-flight accessShared holder to release, which is what
// lets a normal access that started just before migration intent was
// recorded finish safely instead of running against a half-migrated schema.
func (g *accessGate) accessExclusive() (func(), error) {
	unlock, err := flockPath(g.accessLockPath, syscall.LOCK_EX)
	if err != nil {
		return nil, fmt.Errorf("persistence: acquire exclusive access lock: %w", err)
	}
	return unlock, nil
}

// acquireCoordinationExclusive is what a process calls once it has decided
// it must migrate. It is bounded by migrationWait like the shared probe
// above, rather than blocking indefinitely, so a second process that also
// decided to migrate in the same race window fails actionably instead of
// hanging forever if the winner never finishes.
func (g *accessGate) acquireCoordinationExclusive(ctx context.Context) (func(), error) {
	return g.acquireCoordination(ctx, syscall.LOCK_EX)
}

func (g *accessGate) acquireCoordination(ctx context.Context, how int) (func(), error) {
	deadline := time.Now().Add(migrationWait)
	for {
		unlock, ok, err := tryFlockPath(g.coordinationLockPath, how)
		if err != nil {
			return nil, fmt.Errorf("persistence: coordination lock: %w", err)
		}
		if ok {
			return unlock, nil
		}
		if time.Now().After(deadline) {
			return nil, g.migrationInProgressError()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(coordinationPollInterval):
		}
	}
}

func (g *accessGate) migrationInProgressError() error {
	marker, _ := readMarker(g.markerPath)
	if marker != nil {
		return fmt.Errorf("persistence: a migration has been in progress for at least %s (pid %d, started %s, stage %q); wait for it to finish or investigate that process, then retry",
			migrationWait, marker.PID, marker.StartedAt.Format(time.RFC3339), marker.Stage)
	}
	return fmt.Errorf("persistence: a migration has been in progress for at least %s; wait for it to finish, then retry", migrationWait)
}

// migrationMarker is diagnostics only (process, binary, timing, stage, and
// any failure), never a version ledger; the goose ledger inside the
// database remains the sole authority for which migrations are applied.
type migrationMarker struct {
	PID           int       `json:"pid"`
	BinaryVersion string    `json:"binary_version"`
	StartedAt     time.Time `json:"started_at"`
	Stage         string    `json:"stage"`
	Error         string    `json:"error,omitempty"`
}

func writeMarker(path string, m migrationMarker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("persistence: marshal migration marker: %w", err)
	}
	if err := atomicfile.Write(path, data); err != nil {
		return fmt.Errorf("persistence: write migration marker: %w", err)
	}
	return nil
}

func readMarker(path string) (*migrationMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m migrationMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func binaryVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

// flockPath opens (creating) the lock file and blocks until it takes the
// given flock mode, returning an unlock func. Mirrors eventlog.flock; kept
// as its own copy because persistence's lock files sit beside the database
// rather than an event session directory and the two packages have no
// shared lower-level module to hold one copy in.
func flockPath(path string, how int) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// tryFlockPath is flockPath's non-blocking counterpart: ok is false, with a
// nil error, exactly when the lock is currently held by another holder in a
// conflicting mode.
func tryFlockPath(path string, how int) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("flock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}
