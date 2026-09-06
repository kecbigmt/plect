package persistence

import (
	"context"
	"database/sql"
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
