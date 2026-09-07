package persistence

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kecbigmt/plecture/app/internal/version"
)

// fileName is the database's file name within its data directory, matching
// the plect storage vocabulary already in place (`plect storage migrate`/
// `plect storage import`, the SQLite durable storage ADR): every caller that
// needs the path derives it via PathIn rather than joining this literal
// itself, so the name has one source.
const fileName = "storage.db"

// PathIn returns the database path within dir.
func PathIn(dir string) string {
	return filepath.Join(dir, fileName)
}

// DefaultPath is the production database location: $XDG_DATA_HOME/plect/storage.db,
// matching state.NewStore's directory resolution for the sibling state.json.
func DefaultPath() string {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return PathIn(filepath.Join(dataHome, "plect"))
}

// EnsureCurrent is the single entry point every plect process calls before
// doing anything else with the database: `plect` command startup
// (app/commands/root.go), `plect serve` (app/commands/serve.go),
// `plect-web` (app/internal/webui/live.go), and the explicit
// `plect storage migrate` command. It waits out or refuses a concurrent
// migration, refuses a database newer than this binary supports, and
// migrates the schema itself (via Migrate) if this process is the one that
// finds it behind — see docs/design/sqlite-persistence.md's "Migration
// access gate". A development build (version.IsDevelopmentBuild) additionally
// refuses to migrate a database it did not create (schema version > 0)
// forward, rather than migrating it as a release build would; use
// EnsureCurrentAllowDevBuild for the explicit opt-in.
//
// The caller owns the returned DB's lifetime and must Close it.
func EnsureCurrent(ctx context.Context, path string) (*DB, error) {
	return ensureCurrent(ctx, path, migrationsSourceFS(), version.IsDevelopmentBuild(), false)
}

// EnsureCurrentAllowDevBuild is EnsureCurrent, except a development build
// (version.IsDevelopmentBuild) is permitted to forward-migrate a database it
// did not create. `plect storage migrate --allow-dev-build` is its only
// caller; every other entry point uses EnsureCurrent and gets the refusal
// below.
func EnsureCurrentAllowDevBuild(ctx context.Context, path string) (*DB, error) {
	return ensureCurrent(ctx, path, migrationsSourceFS(), version.IsDevelopmentBuild(), true)
}

// ensureCurrent is EnsureCurrent with an injectable migration source and an
// injectable dev-build determination, so this package's own tests can
// exercise interrupted and concurrent migrations against a small,
// controllable migration set, and the dev-build refusal itself, without
// depending on the real migrations/ tree or the process's actual build
// stamp.
func ensureCurrent(ctx context.Context, path string, migrations fs.FS, isDevBuild, allowDevBuild bool) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("persistence: create database directory: %w", err)
	}

	// Open serializes its own first-touch Ping against a concurrent first
	// touch or migration; version below goes through the same normal-access
	// protocol (enterShared) as every other read.
	db, err := Open(path)
	if err != nil {
		return nil, err
	}
	db.migrations = migrations

	current, target, err := db.version(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := refuseIfNewerThanSupported(current, target); err != nil {
		db.Close()
		return nil, err
	}
	if current == target {
		return db, nil
	}

	// current > 0 means some earlier process already brought this database
	// to a non-zero schema — a store this process did not create. current
	// == 0 means this call is the one creating it, which carries none of
	// the incident's risk (there is no pre-existing host data to strand).
	if current > 0 && isDevBuild && !allowDevBuild {
		db.Close()
		return nil, fmt.Errorf("persistence: %s is at schema %d; this development build would migrate it to %d.\n"+
			"Refusing: point XDG_DATA_HOME at a scratch directory, or run `plect storage migrate --allow-dev-build` deliberately.",
			filepath.Base(path), current, target)
	}

	if err := db.Migrate(ctx); err != nil {
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
