package persistence

import (
	"context"
	"database/sql"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A deferred transaction (SQLite's default) takes no lock until its first
// statement runs, so an empty callback would never block a concurrent
// writer. WithImmediateTx must instead reserve the writer lock the moment
// the transaction begins — before fn runs any statement — precisely so a
// later read-then-write inside fn can never be invalidated by another
// writer's commit landing in between. This test proves that begin-time
// reservation directly: a concurrent write attempt blocks on an empty,
// not-yet-executed callback, and only proceeds once the first callback
// returns.
func TestWithImmediateTx_ReservesWriterLockBeforeFnRunsAnyStatement(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	const hold = 400 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})

	firstErr := make(chan error, 1)
	go func() {
		firstErr <- db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first WithImmediateTx never reached its callback")
	}

	time.AfterFunc(hold, func() { close(release) })

	start := time.Now()
	secondErr := db.WithImmediateTx(ctx, func(tx *sql.Tx) error { return nil })
	elapsed := time.Since(start)

	if secondErr != nil {
		t.Fatalf("second WithImmediateTx: %v", secondErr)
	}
	if err := <-firstErr; err != nil {
		t.Fatalf("first WithImmediateTx: %v", err)
	}
	if elapsed < hold/2 {
		t.Errorf("second WithImmediateTx returned after %s, want it blocked for roughly %s while the first held the writer lock", elapsed, hold)
	}
}

func TestWithImmediateTx_WaitsForMigrationIntentBeforeEnteringCallback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	unlockCoord, ok, err := tryFlockPath(db.gate.coordinationLockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatalf("tryFlockPath: %v", err)
	}
	if !ok {
		t.Fatal("tryFlockPath did not acquire the uncontended coordination lock")
	}

	entered := make(chan struct{})
	txErr := make(chan error, 1)
	go func() {
		txErr <- db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
			close(entered)
			return nil
		})
	}()

	select {
	case <-entered:
		t.Fatal("WithImmediateTx's callback ran while a migrator held the coordination lock (recorded intent)")
	case <-time.After(300 * time.Millisecond):
		// expected: still waiting behind the coordination lock
	}

	unlockCoord()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("WithImmediateTx never proceeded after the coordination lock was released")
	}
	if err := <-txErr; err != nil {
		t.Fatalf("WithImmediateTx: %v", err)
	}
}

// An already-open *DB does not itself observe a migration another handle
// applies after it opened; only a fresh version check would. This proves
// WithImmediateTx performs that check on every call rather than trusting
// whatever EnsureCurrent last confirmed.
func TestWithImmediateTx_RefusesOnceAnotherHandleMigratesPastWhatThisOneSupports(t *testing.T) {
	ctx := context.Background()
	path := testDBPath(t)
	older := migrationFixture(map[string]string{"00001_a.sql": migrationA})
	newer := migrationFixture(map[string]string{"00001_a.sql": migrationA, "00002_b.sql": migrationBOK})

	oldHandle, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer oldHandle.Close()
	oldHandle.migrations = older
	if err := oldHandle.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (old handle, to version 1): %v", err)
	}

	newHandle, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer newHandle.Close()
	newHandle.migrations = newer
	if err := newHandle.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (new handle, to version 2): %v", err)
	}

	entered := false
	err = oldHandle.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		entered = true
		return nil
	})
	if err == nil {
		t.Fatal("WithImmediateTx on a handle whose migrations stop at version 1 unexpectedly succeeded against a version-2 ledger")
	}
	if entered {
		t.Error("WithImmediateTx's callback ran against a ledger version this handle does not support")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error = %q, want it to mention the binary does not support the ledger's version", err)
	}
}

func TestWithImmediateTx_RollsBackOnFnError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	wantErr := context.Canceled
	err := db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO persistence_smoke (note, created_at) VALUES (?, ?)", "rolled back", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("exec: %v", err)
		}
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("WithImmediateTx error = %v, want %v", err, wantErr)
	}

	var count int
	if err := db.write.QueryRowContext(ctx, "SELECT COUNT(*) FROM persistence_smoke").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("persistence_smoke has %d rows, want 0 (fn's error should have rolled back the insert)", count)
	}
}
