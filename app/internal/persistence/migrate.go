package persistence

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

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

// Migrate defers entirely to goose's own applied-version ledger rather
// than tracking a second record of what has run, so it is safe to call on
// every startup: goose.Provider.Up is a no-op once the ledger shows every
// embedded migration already applied.
//
// It holds the access gate's exclusive lock for the whole call, which is
// what makes it safe to call directly (as this package's own tests do) as
// well as through EnsureCurrent's coordinated runner: either way, no normal
// access can observe a half-migrated schema, because accessExclusive waits
// out every already-in-flight accessShared holder first. migrateAsRunner
// (ensure.go) already holds accessExclusive itself by the time it needs to
// apply migrations, so it calls migrateLocked directly — calling Migrate
// there would request accessExclusive a second time on a different file
// descriptor for the same lock file and deadlock against its own hold,
// since flock is scoped to the open file description, not the process.
func (db *DB) Migrate(ctx context.Context) error {
	unlock, err := db.gate.accessExclusive()
	if err != nil {
		return err
	}
	defer unlock()

	return db.migrateLocked(ctx)
}

// migrateLocked is Migrate's body without acquiring the access lock itself;
// see Migrate's doc comment for why a caller that already holds
// accessExclusive must call this instead.
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
// highest version this binary's migration source declares. It goes through
// enterShared, like any other normal access, so a call racing a migrator
// that has already recorded intent waits (or refuses) instead of reading
// through to a schema that is about to change.
func (db *DB) version(ctx context.Context) (current, target int64, err error) {
	unlock, err := db.gate.enterShared(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer unlock()

	return db.versionLocked(ctx)
}

// versionLocked is version's body without acquiring the access gate
// itself; migrateAsRunner (ensure.go) calls it directly while already
// holding accessExclusive, for the same self-deadlock reason documented on
// Migrate.
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
