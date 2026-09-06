package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// eventTablesVersion is the goose version immediately before this package's
// events.delivery_mode column drop — the last migration where a row can
// still be inserted with that column present.
const eventTablesVersion = 20260906143514

// TestMigrate_DropsDeliveryModeColumnWithoutLosingExistingRows is the
// upgrade-path regression the empty-database schema-equivalence check
// cannot exercise: it seeds a row under the pre-drop schema (delivery_mode
// present, as a real database that predates this change would have), then
// applies the drop-column migration and proves every other column survived
// — not just that a freshly-created database ends up with the right shape.
func TestMigrate_DropsDeliveryModeColumnWithoutLosingExistingRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.UpTo(ctx, eventTablesVersion); err != nil {
		t.Fatalf("UpTo(%d): %v", eventTablesVersion, err)
	}

	const streamID, session = "01STREAM0000000000000000", "s1"
	if _, err := db.write.ExecContext(ctx,
		`INSERT INTO event_streams (id, session_name, created_at) VALUES (?, ?, ?)`,
		streamID, session, formatTime(time.Now()),
	); err != nil {
		t.Fatalf("seed event_streams: %v", err)
	}
	if _, err := db.write.ExecContext(ctx,
		`INSERT INTO events (id, stream_id, sequence, time, type, source, direction, summary, body, metadata_json, delivery_mode)
		 VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"01EVENT0000000000000000", streamID, formatTime(time.Now()),
		"widget.message", "widget", "inbound", "hello", "hello body", "{}", "push",
	); err != nil {
		t.Fatalf("seed pre-migration event row: %v", err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to current: %v", err)
	}

	var count int
	if err := db.write.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = 'delivery_mode'`).Scan(&count); err != nil {
		t.Fatalf("check delivery_mode column: %v", err)
	}
	if count != 0 {
		t.Fatalf("events.delivery_mode still present after migration")
	}

	evs, seqs, err := db.ListEventsFrom(ctx, session, 0)
	if err != nil {
		t.Fatalf("ListEventsFrom: %v", err)
	}
	if len(evs) != 1 || len(seqs) != 1 {
		t.Fatalf("got %d events, want the pre-migration row preserved", len(evs))
	}
	got := evs[0]
	if got.ID != "01EVENT0000000000000000" || got.Type != "widget.message" || got.Source != "widget" ||
		string(got.Direction) != "inbound" || got.Summary != "hello" || got.Body != "hello body" {
		t.Fatalf("pre-migration row's surviving columns = %+v, want the seeded values intact", got)
	}
	if seqs[0] != 1 {
		t.Fatalf("sequence = %d, want 1 (unaffected by the column drop)", seqs[0])
	}
}
