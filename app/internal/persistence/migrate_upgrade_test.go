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

// seedEventRow uses raw SQL rather than AppendEvent: AppendEvent's generated
// insert targets only the current, already-migrated column set.
func seedEventRow(t *testing.T, db *DB, ctx context.Context, streamID, session, eventID string, when time.Time, withDeliveryMode bool) {
	t.Helper()
	if _, err := db.write.ExecContext(ctx,
		`INSERT INTO event_streams (id, session_name, created_at) VALUES (?, ?, ?)`,
		streamID, session, formatTime(when),
	); err != nil {
		t.Fatalf("seed event_streams: %v", err)
	}
	columns := "id, stream_id, sequence, time, type, source, direction, summary, body, metadata_json"
	args := []any{eventID, streamID, formatTime(when), "widget.message", "widget", "inbound", "hello", "hello body", `{"k":"v"}`}
	if withDeliveryMode {
		columns += ", delivery_mode"
		args = append(args, "push")
	}
	placeholders := "?, ?, 1, ?, ?, ?, ?, ?, ?, ?"
	if withDeliveryMode {
		placeholders += ", ?"
	}
	if _, err := db.write.ExecContext(ctx,
		"INSERT INTO events ("+columns+") VALUES ("+placeholders+")", args...,
	); err != nil {
		t.Fatalf("seed event row: %v", err)
	}
}

// assertEventRowIntact checks every retained field, not just id and type: a
// column-drop migration that silently truncated or reordered one would
// otherwise pass unnoticed.
func assertEventRowIntact(t *testing.T, db *DB, ctx context.Context, session, eventID string, when time.Time) {
	t.Helper()
	evs, seqs, err := db.ListEventsFrom(ctx, session, 0)
	if err != nil {
		t.Fatalf("ListEventsFrom: %v", err)
	}
	if len(evs) != 1 || len(seqs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	got := evs[0]
	if got.ID != eventID || got.Type != "widget.message" || got.Source != "widget" ||
		string(got.Direction) != "inbound" || got.Summary != "hello" || got.Body != "hello body" {
		t.Fatalf("surviving columns = %+v, want the seeded values intact", got)
	}
	if !got.Time.Equal(when) {
		t.Errorf("Time = %v, want %v", got.Time, when)
	}
	if got.Metadata["k"] != "v" {
		t.Errorf("Metadata = %v, want k=v", got.Metadata)
	}
	if seqs[0] != 1 {
		t.Fatalf("sequence = %d, want 1", seqs[0])
	}
}

func deliveryModeColumnCount(t *testing.T, db *DB, ctx context.Context) int {
	t.Helper()
	var count int
	if err := db.write.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = 'delivery_mode'`,
	).Scan(&count); err != nil {
		t.Fatalf("check delivery_mode column: %v", err)
	}
	return count
}

// TestMigrate_DropsDeliveryModeColumnWithoutLosingExistingRows is the
// upgrade-path case TestSchemaSQL_MatchesMigrationHistory cannot cover: that
// check only ever migrates an empty database.
func TestMigrate_DropsDeliveryModeColumnWithoutLosingExistingRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	when := time.Now().UTC().Truncate(time.Second)

	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.UpTo(ctx, eventTablesVersion); err != nil {
		t.Fatalf("UpTo(%d): %v", eventTablesVersion, err)
	}
	seedEventRow(t, db, ctx, "01STREAM0000000000000000", "s1", "01EVENT0000000000000000", when, true)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to current: %v", err)
	}

	if got := deliveryModeColumnCount(t, db, ctx); got != 0 {
		t.Fatalf("events.delivery_mode still present after migration")
	}
	assertEventRowIntact(t, db, ctx, "s1", "01EVENT0000000000000000", when)
}

// TestMigrate_DownRestoresDeliveryModeColumnWithoutError is the bug-fix
// regression: Atlas's own generated Down for this drop referenced the
// table's pre-rename temporary name, which no longer exists once Up
// finishes, so stepping back would error rather than restore the column.
func TestMigrate_DownRestoresDeliveryModeColumnWithoutError(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	when := time.Now().UTC().Truncate(time.Second)

	seedEventRow(t, db, ctx, "01STREAM0000000000000001", "s2", "01EVENT0000000000000001", when, false)

	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}

	if got := deliveryModeColumnCount(t, db, ctx); got != 1 {
		t.Fatalf("events.delivery_mode not restored by Down")
	}
	var deliveryMode string
	if err := db.write.QueryRowContext(ctx, `SELECT delivery_mode FROM events WHERE id = ?`, "01EVENT0000000000000001").Scan(&deliveryMode); err != nil {
		t.Fatalf("read restored delivery_mode: %v", err)
	}
	if deliveryMode != "" {
		t.Errorf("delivery_mode = %q, want the added column's default empty value for a row that predates Down", deliveryMode)
	}
	assertEventRowIntact(t, db, ctx, "s2", "01EVENT0000000000000001", when)
}
