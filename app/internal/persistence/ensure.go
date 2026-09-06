package persistence

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DefaultPath is the production database location: $XDG_DATA_HOME/plect/store.db,
// matching state.NewStore's directory resolution for the sibling state.json.
func DefaultPath() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "plect", "store.db")
}

// EnsureCurrent is the single entry point every plect process calls before
// doing anything else with the database: `plect` command startup
// (app/commands/root.go), `plect serve` (app/commands/serve.go),
// `plect-web` (app/internal/webui/live.go), and the explicit
// `plect storage migrate` command. It waits out or refuses a concurrent
// migration, refuses a database newer than this binary supports, and
// migrates the schema itself if this process is the one that finds it
// behind — see docs/design/sqlite-persistence.md's "Migration access gate".
//
// The caller owns the returned DB's lifetime and must Close it.
func EnsureCurrent(ctx context.Context, path string) (*DB, error) {
	return ensureCurrent(ctx, path, migrationsSourceFS())
}

// ensureCurrent is EnsureCurrent with an injectable migration source, so
// this package's own tests can exercise interrupted and concurrent
// migrations against a small, controllable migration set instead of the
// real migrations/ tree.
func ensureCurrent(ctx context.Context, path string, migrations fs.FS) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("persistence: create database directory: %w", err)
	}

	gate := newAccessGate(path)

	// enterShared, not just the coordination probe: Open below (and the
	// Ping it performs) is itself an interaction with store.db, so it must
	// happen only once this process holds the access gate like any other
	// normal operation, not before any lock is taken.
	unlockShared, err := gate.enterShared(ctx)
	if err != nil {
		return nil, err
	}

	db, err := Open(path)
	if err != nil {
		unlockShared()
		return nil, err
	}
	db.migrations = migrations

	current, target, err := db.versionLocked(ctx)
	if err != nil {
		unlockShared()
		db.Close()
		return nil, err
	}
	if err := refuseIfNewerThanSupported(current, target); err != nil {
		unlockShared()
		db.Close()
		return nil, err
	}
	if current == target {
		unlockShared()
		return db, nil
	}
	unlockShared()

	if err := db.migrateAsRunner(ctx, gate); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func refuseIfNewerThanSupported(current, target int64) error {
	if current <= target {
		return nil
	}
	return fmt.Errorf("persistence: database schema version %d is newer than this binary supports (max %d); use a newer plect binary, this one makes no change", current, target)
}

// migrateAsRunner is what a process calls once it has found the schema
// behind and decided it must be the one to bring it current. It acquires
// both exclusive locks — coordination first (so a second process racing to
// migrate waits behind this one instead of both applying migrations at
// once), then access (which waits out every already-in-flight accessShared
// holder) — before it recheck-and-migrates, so no interaction with store.db
// beyond this point can race a normal access that arrives after intent was
// recorded.
func (db *DB) migrateAsRunner(ctx context.Context, gate *accessGate) error {
	unlockCoord, err := gate.acquireCoordinationExclusive(ctx)
	if err != nil {
		return err
	}
	defer unlockCoord()

	unlockAccess, err := gate.accessExclusive()
	if err != nil {
		return err
	}
	defer unlockAccess()

	// Recheck under exclusion: another process may have already migrated
	// between this process's first check and now obtaining both exclusive
	// locks, in which case there is nothing left to do. versionLocked, not
	// version: this call already holds both locks, and version's own
	// enterShared would try to re-acquire the coordination and access locks
	// on separate file descriptors for the same lock files and deadlock
	// against its own hold — flock is scoped to the open file description,
	// not the process.
	current, target, err := db.versionLocked(ctx)
	if err != nil {
		return err
	}
	if err := refuseIfNewerThanSupported(current, target); err != nil {
		return err
	}
	if current == target {
		return nil
	}

	marker := migrationMarker{
		PID:           os.Getpid(),
		BinaryVersion: binaryVersion(),
		StartedAt:     time.Now().UTC(),
		Stage:         "migrating",
	}
	if err := writeMarker(gate.markerPath, marker); err != nil {
		return err
	}

	if migrateErr := db.migrateLocked(ctx); migrateErr != nil {
		marker.Stage = "failed"
		marker.Error = migrateErr.Error()
		if writeErr := writeMarker(gate.markerPath, marker); writeErr != nil {
			return fmt.Errorf("%w (also failed to record failure evidence: %v)", migrateErr, writeErr)
		}
		return fmt.Errorf("persistence: migration failed, pre-migration state and failure evidence preserved at %s: %w", gate.markerPath, migrateErr)
	}

	if err := os.Remove(gate.markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("persistence: remove migration marker after success: %w", err)
	}
	return nil
}
