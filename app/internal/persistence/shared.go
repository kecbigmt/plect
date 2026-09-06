package persistence

import (
	"context"
	"sync"
)

// shared memoizes one *DB per database path across every caller in the process, so state.Store and eventlog.Store opened over the same file draw on one connection pool instead of each opening (and never closing) their own.
var (
	sharedMu sync.Mutex
	shared   = map[string]*DB{}
)

// EnsureCurrentShared is EnsureCurrent, memoized per path until CloseShared releases it.
func EnsureCurrentShared(ctx context.Context, path string) (*DB, error) {
	sharedMu.Lock()
	if db, ok := shared[path]; ok {
		sharedMu.Unlock()
		return db, nil
	}
	sharedMu.Unlock()

	db, err := EnsureCurrent(ctx, path)
	if err != nil {
		return nil, err
	}

	sharedMu.Lock()
	defer sharedMu.Unlock()
	if existing, ok := shared[path]; ok {
		// Lost the race to open path first: close the redundant connection.
		db.Close()
		return existing, nil
	}
	shared[path] = db
	return db, nil
}

// CloseShared closes path's shared database, if one is open, and forgets it so a later EnsureCurrentShared call reopens fresh — the deterministic owner's shutdown-time counterpart, called once every consumer sharing the handle has stopped using it.
func CloseShared(path string) error {
	sharedMu.Lock()
	db, ok := shared[path]
	delete(shared, path)
	sharedMu.Unlock()
	if !ok {
		return nil
	}
	return db.Close()
}
