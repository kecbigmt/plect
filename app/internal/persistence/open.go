package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	_ "github.com/mattn/go-sqlite3"
)

// busyTimeoutMillis bounds how long a connection retries against
// SQLITE_BUSY before giving up. It applies to every connection this
// package opens; a caller waiting longer than this on lock contention gets
// an actionable error instead of blocking indefinitely.
const busyTimeoutMillis = 5000

// DB is one SQLite database opened per this package's connection
// configuration. It holds two connection pools against the same file: read
// begins each transaction with the default deferred lock, and write begins
// each transaction with BEGIN IMMEDIATE via WithImmediateTx. Splitting them
// is the only way to give write transactions BEGIN IMMEDIATE without also
// forcing it onto read-only access, because go-sqlite3 fixes a connection's
// transaction-lock mode at open time (the "_txlock" DSN parameter), not per
// transaction.
type DB struct {
	read  *sql.DB
	write *sql.DB

	// gate is this database's migration access gate (see gate.go),
	// derived from its file path. Migrate and WithImmediateTx both go
	// through it so every later slice inherits the gating for free.
	gate *accessGate

	// migrations is the goose migration source. It defaults to the
	// embedded production tree; tests in this package override it via
	// direct field assignment (same package) to exercise interrupted and
	// concurrent migrations without touching the real migrations/ tree.
	migrations fs.FS
}

// Open sets WAL journaling, a bounded busy timeout, and foreign-key
// enforcement as DSN parameters rather than leaving them to each caller,
// so every connection this package ever opens carries them, with no path
// through Open that could construct a connection missing one. It does not
// apply migrations; call Migrate for that.
func Open(path string) (*DB, error) {
	readDSN := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=%d&_foreign_keys=on", path, busyTimeoutMillis)
	writeDSN := readDSN + "&_txlock=immediate"

	read, err := sql.Open("sqlite3", readDSN)
	if err != nil {
		return nil, fmt.Errorf("open read handle: %w", err)
	}
	write, err := sql.Open("sqlite3", writeDSN)
	if err != nil {
		read.Close()
		return nil, fmt.Errorf("open write handle: %w", err)
	}
	// SQLite allows exactly one writer at a time regardless of how many
	// connections request one; capping the pool at one avoids opening
	// redundant connections that would only ever queue behind each other.
	write.SetMaxOpenConns(1)

	if err := read.Ping(); err != nil {
		read.Close()
		write.Close()
		return nil, fmt.Errorf("ping read handle: %w", err)
	}
	if err := write.Ping(); err != nil {
		read.Close()
		write.Close()
		return nil, fmt.Errorf("ping write handle: %w", err)
	}

	return &DB{read: read, write: write, gate: newAccessGate(path), migrations: migrationsSourceFS()}, nil
}

func (db *DB) Close() error {
	readErr := db.read.Close()
	writeErr := db.write.Close()
	if readErr != nil {
		return readErr
	}
	return writeErr
}

// WithImmediateTx runs fn inside a write transaction that begins with
// BEGIN IMMEDIATE: it reserves the single SQLite writer lock before fn
// executes any statement, so a read fn performs before a later write in
// the same callback can never be invalidated by another writer committing
// in between (the failure a deferred transaction's lazy lock upgrade would
// hit instead). The transaction commits if fn returns nil and rolls back
// otherwise; if the rollback itself also fails, that failure is appended
// to fn's error rather than replacing it, so a genuine fn error is never
// masked by a rollback failure.
func (db *DB) WithImmediateTx(ctx context.Context, fn func(*sql.Tx) error) error {
	unlock, err := db.gate.accessShared()
	if err != nil {
		return err
	}
	defer unlock()

	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin immediate transaction: %w", err)
	}

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit immediate transaction: %w", err)
	}
	return nil
}
