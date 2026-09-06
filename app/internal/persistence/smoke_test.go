package persistence

import (
	"context"
	"testing"
	"time"
)

func TestInsertSmoke_RoundTripsThroughGeneratedQuery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	before := time.Now().Add(-time.Second)
	record, err := db.InsertSmoke(ctx, "hello, persistence")
	if err != nil {
		t.Fatalf("InsertSmoke: %v", err)
	}

	if record.ID == 0 {
		t.Errorf("record.ID = 0, want an assigned rowid")
	}
	if record.Note != "hello, persistence" {
		t.Errorf("record.Note = %q, want %q", record.Note, "hello, persistence")
	}
	if record.CreatedAt.Before(before) {
		t.Errorf("record.CreatedAt = %s, want it after %s", record.CreatedAt, before)
	}
}
