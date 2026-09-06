package persistence

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

// Both pools are asserted on directly (rather than only through the public
// API) because DB deliberately opens two separate connection pools against
// the same file (see DB's doc comment) — a regression that applies the
// production pragmas to only one of them would not show up if only one
// pool were ever queried.
func TestOpen_BothHandlesHaveProductionPragmas(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for name, handle := range map[string]*sql.DB{"read": db.read, "write": db.write} {
		t.Run(name, func(t *testing.T) {
			assertPragma(t, ctx, handle, "journal_mode", "wal")
			assertPragma(t, ctx, handle, "foreign_keys", "1")
			assertBusyTimeoutIsBoundedAndNonZero(t, ctx, handle)
		})
	}
}

func assertPragma(t *testing.T, ctx context.Context, handle *sql.DB, pragma, want string) {
	t.Helper()
	var got string
	if err := handle.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", pragma, err)
	}
	if got != want {
		t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
	}
}

func assertBusyTimeoutIsBoundedAndNonZero(t *testing.T, ctx context.Context, handle *sql.DB) {
	t.Helper()
	var ms int
	if err := handle.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if ms <= 0 {
		t.Errorf("PRAGMA busy_timeout = %d, want a positive (bounded, non-zero) value", ms)
	}
}
