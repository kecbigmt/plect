package persistence

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func migrationsSourceFS() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		// migrationsFS is compiled in via the go:embed directive above;
		// a missing "migrations" subtree would be a build-time packaging
		// bug, not a runtime condition a caller can recover from.
		panic(fmt.Sprintf("persistence: embedded migrations tree missing: %v", err))
	}
	return sub
}

// Migrate is the migration runner protocol in full — the only path that is
// allowed to change store.db's schema, whether called directly (as this
// package's own tests do for setup) or from EnsureCurrent: it records
// intent by acquiring the coordination lock exclusively, waits out any
// already-in-flight normal access by then acquiring the access lock
// exclusively, rechecks the ledger under that exclusion (another process
// may have already migrated, or the ledger may already be newer than this
// binary supports), and preserves diagnostic evidence in the marker file
// across a failure. It defers entirely to goose's own applied-version
// ledger rather than tracking a second record of what has run, so it is
// safe to call on every startup: goose.Provider.Up is a no-op once the
// ledger shows every embedded migration already applied.
func (db *DB) Migrate(ctx context.Context) error {
	gate := db.gate

	unlockCoord, err := gate.acquireCoordinationExclusive(ctx)
	if err != nil {
		return err
	}
	defer unlockCoord()

	// Written and fsynced immediately, before accessExclusive below (which
	// can wait out an in-flight normal access for a while): per design,
	// intent is recorded the moment this process commits to migrating —
	// i.e. the moment it holds the coordination lock exclusively — not
	// once it has also finished waiting for the access lock. Otherwise a
	// process that times out waiting behind this coordination hold sees no
	// diagnostic detail, and this process exiting during that wait would
	// leave no evidence at all.
	marker := migrationMarker{
		PID:           os.Getpid(),
		BinaryVersion: binaryVersion(),
		StartedAt:     time.Now().UTC(),
		Stage:         "migrating",
	}
	if err := writeMarker(gate.markerPath, marker); err != nil {
		return err
	}

	unlockAccess, err := gate.accessExclusive()
	if err != nil {
		return err
	}
	defer unlockAccess()

	current, target, err := db.versionLocked(ctx)
	if err != nil {
		return err
	}
	if err := refuseIfNewerThanSupported(current, target); err != nil {
		if removeErr := removeMarker(gate.markerPath); removeErr != nil {
			return fmt.Errorf("%w (also failed to remove the marker recorded on intent: %v)", err, removeErr)
		}
		return err
	}
	if current == target {
		// Another process already migrated between this one recording
		// intent and reaching exclusion; nothing to do, so the marker this
		// process speculatively wrote is removed rather than left behind.
		return removeMarker(gate.markerPath)
	}

	if migrateErr := db.migrateLocked(ctx); migrateErr != nil {
		marker.Stage = "failed"
		marker.Error = migrateErr.Error()
		if writeErr := writeMarker(gate.markerPath, marker); writeErr != nil {
			return fmt.Errorf("%w (also failed to record failure evidence: %v)", migrateErr, writeErr)
		}
		return fmt.Errorf("persistence: migration failed, pre-migration state and failure evidence preserved at %s: %w", gate.markerPath, migrateErr)
	}

	return removeMarker(gate.markerPath)
}

func removeMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("persistence: remove migration marker: %w", err)
	}
	return nil
}

// migrateLocked applies pending migrations without acquiring any lock
// itself; only Migrate calls it, already holding accessExclusive.
func (db *DB) migrateLocked(ctx context.Context) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// version returns the goose ledger's currently applied version and the
// highest version this binary's migration source declares, via
// enterShared like any other normal access.
func (db *DB) version(ctx context.Context) (current, target int64, err error) {
	unlock, err := db.gate.enterShared(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer unlock()

	return db.versionLocked(ctx)
}

// versionLocked is version's body without acquiring the access gate
// itself; Migrate calls it directly while already holding accessExclusive.
func (db *DB) versionLocked(ctx context.Context) (current, target int64, err error) {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		return 0, 0, fmt.Errorf("create migration provider: %w", err)
	}
	current, err = provider.GetDBVersion(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("read applied migration version: %w", err)
	}
	for _, source := range provider.ListSources() {
		if source.Version > target {
			target = source.Version
		}
	}
	return current, target, nil
}

// Version returns the goose ledger's currently applied migration version.
func (db *DB) Version(ctx context.Context) (int64, error) {
	current, _, err := db.version(ctx)
	return current, err
}

// enterNormalAccess is what every normal query and write transaction goes
// through — the access gate's own enterShared, plus a recheck that this
// handle's migration source still covers the ledger's current version. The
// gate alone is not enough: a *DB opened while at the latest version can
// stay open (and keep passing the gate's locking) across a later migration
// applied by another process entirely, so each access must also confirm
// its own binary still understands the schema, not just that no migration
// is currently in flight.
func (db *DB) enterNormalAccess(ctx context.Context) (func(), error) {
	unlock, err := db.gate.enterShared(ctx)
	if err != nil {
		return nil, err
	}
	current, target, err := db.versionLocked(ctx)
	if err != nil {
		unlock()
		return nil, err
	}
	if err := refuseIfNewerThanSupported(current, target); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}
