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
// It refuses a database whose ledger already names a migration newer than
// anything this binary embeds, rather than silently leaving it as-is or
// attempting to apply an older binary's migration set on top of it. The
// full cross-process migration-exclusion gate (advisory lock files, a
// diagnostics marker) is a later slice; this method only takes a plain
// flock around the migration itself, because two processes racing to
// create the same brand-new database's tables both observe "nothing
// applied yet" and then collide on CREATE TABLE — see migrate_test.go.
func (db *DB) Migrate(ctx context.Context) error {
	return withFileLock(db.path+".migrate.lock", func() error {
		provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, migrationsSourceFS())
		if err != nil {
			return fmt.Errorf("create migration provider: %w", err)
		}

		dbVersion, err := provider.GetDBVersion(ctx)
		if err != nil {
			return fmt.Errorf("read applied migration version: %w", err)
		}
		var maxKnown int64
		for _, source := range provider.ListSources() {
			if source.Version > maxKnown {
				maxKnown = source.Version
			}
		}
		if dbVersion > maxKnown {
			return fmt.Errorf("persistence: database schema version %d is newer than this binary supports (%d); use a matching or newer plect binary", dbVersion, maxKnown)
		}

		if _, err := provider.Up(ctx); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		return nil
	})
}
