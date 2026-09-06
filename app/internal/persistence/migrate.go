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
// out every already-in-flight accessShared holder first.
func (db *DB) Migrate(ctx context.Context) error {
	unlock, err := db.gate.accessExclusive()
	if err != nil {
		return err
	}
	defer unlock()

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
// highest version this binary's migration source declares. It takes the
// access gate shared, like any other read, so it never blocks or is blocked
// by anything other than an in-progress migration.
func (db *DB) version(ctx context.Context) (current, target int64, err error) {
	unlock, err := db.gate.accessShared()
	if err != nil {
		return 0, 0, err
	}
	defer unlock()

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
