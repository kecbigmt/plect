package persistence

import (
	"context"
	"database/sql"
	"testing"
)

// TestWithReadTx_HoldsOneSnapshotAcrossQueries proves the guarantee
// GetSession, AllSessions, FindSessionsByAlias, and Population all depend
// on: two queries inside the same WithReadTx callback see the same
// snapshot even when a concurrent writer commits between them. Without
// this, a caller composing one logical value from separate autocommit
// queries (a session's base row, then its children/tasks) could observe a
// value that never existed at any single point in time.
func TestWithReadTx_HoldsOneSnapshotAcrossQueries(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if err := db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO persistence_smoke (note, created_at) VALUES (?, ?)", "seed", "2024-01-01T00:00:00Z")
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	firstQueryDone := make(chan struct{})
	writerDone := make(chan struct{})
	readTxDone := make(chan struct{})
	var firstCount, secondCount int
	var readErr error

	go func() {
		defer close(readTxDone)
		readErr = db.WithReadTx(ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM persistence_smoke").Scan(&firstCount); err != nil {
				return err
			}
			close(firstQueryDone)
			<-writerDone
			return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM persistence_smoke").Scan(&secondCount)
		})
	}()

	<-firstQueryDone
	if err := db.WithImmediateTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO persistence_smoke (note, created_at) VALUES (?, ?)", "concurrent", "2024-01-01T00:00:01Z")
		return err
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	close(writerDone)
	<-readTxDone

	if readErr != nil {
		t.Fatalf("WithReadTx: %v", readErr)
	}
	if firstCount != 1 {
		t.Fatalf("firstCount = %d, want 1", firstCount)
	}
	if secondCount != firstCount {
		t.Fatalf("secondCount = %d, want %d (the concurrent write must not be visible inside the same read transaction)", secondCount, firstCount)
	}

	// The write is visible to a fresh read once the read transaction ends.
	var afterCount int
	if err := db.WithReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM persistence_smoke").Scan(&afterCount)
	}); err != nil {
		t.Fatalf("post-check WithReadTx: %v", err)
	}
	if afterCount != 2 {
		t.Fatalf("afterCount = %d, want 2 (a new read transaction must see the committed write)", afterCount)
	}
}
