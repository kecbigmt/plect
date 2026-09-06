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
func (db *DB) Migrate(ctx context.Context) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, migrationsSourceFS())
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
