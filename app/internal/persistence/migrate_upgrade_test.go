package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
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
// It stops one migration short of current: the record_json-dissolution
// migration on top re-keys events by session_id, which this test's raw SQL
// predates (see TestMigrate_DownRestoresPreDissolutionSchemaWithoutError).
func TestMigrate_DownRestoresDeliveryModeColumnWithoutError(t *testing.T) {
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
	if _, err := provider.UpByOne(ctx); err != nil {
		t.Fatalf("UpByOne (apply delivery-mode drop): %v", err)
	}
	seedEventRow(t, db, ctx, "01STREAM0000000000000001", "s2", "01EVENT0000000000000001", when, false)

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
	// Raw SQL: this database predates session_id, so the generated queries don't apply.
	var gotType, gotSummary, gotBody string
	if err := db.write.QueryRowContext(ctx, `SELECT type, summary, body FROM events WHERE id = ?`, "01EVENT0000000000000001").Scan(&gotType, &gotSummary, &gotBody); err != nil {
		t.Fatalf("read restored event row: %v", err)
	}
	if gotType != "widget.message" || gotSummary != "hello" || gotBody != "hello body" {
		t.Fatalf("restored event = (%q, %q, %q), want the seeded values intact", gotType, gotSummary, gotBody)
	}
}

// TestMigrate_DownRestoresPreDissolutionSchemaWithoutError proves the
// dissolution migration's Down runs without error and leaves an
// identifiable event row reachable under the restored pre-dissolution
// schema. It does not assert losslessness -- see that migration's own
// header comment. It steps down twice: once for the node-execution-identity
// migration now on top, then once for the dissolution migration itself.
func TestMigrate_DownRestoresPreDissolutionSchemaWithoutError(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := db.PutSession(ctx, &domain.Session{Name: "s3", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "01EVENT0000000000000002", SessionName: "s3", Time: now, Type: "widget.message", Source: "widget", Direction: event.Inbound, Summary: "hello"}); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down (node-execution-identity): %v", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down (dissolution): %v", err)
	}

	var count int
	if err := db.write.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'event_streams'`).Scan(&count); err != nil {
		t.Fatalf("check event_streams table: %v", err)
	}
	if count != 1 {
		t.Fatal("Down did not restore the event_streams table")
	}
	var recordJSONCount int
	if err := db.write.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'record_json'`).Scan(&recordJSONCount); err != nil {
		t.Fatalf("check sessions.record_json column: %v", err)
	}
	if recordJSONCount != 1 {
		t.Fatal("Down did not restore sessions.record_json")
	}

	var gotID, gotType, gotSummary string
	if err := db.write.QueryRowContext(ctx,
		`SELECT e.id, e.type, e.summary FROM events e
		 JOIN event_streams es ON es.id = e.stream_id
		 WHERE es.session_name = ?`, "s3",
	).Scan(&gotID, &gotType, &gotSummary); err != nil {
		t.Fatalf("read restored event row: %v", err)
	}
	if gotID != "01EVENT0000000000000002" || gotType != "widget.message" || gotSummary != "hello" {
		t.Fatalf("restored event = (%q, %q, %q), want the seeded values intact", gotID, gotType, gotSummary)
	}
}

// preNodeExecutionIdentityVersion is the last migration where a
// node_instances row still carries its own status/inputs/outputs directly.
const preNodeExecutionIdentityVersion = 20260907002408

// Covers the upgrade path TestSchemaSQL_MatchesMigrationHistory cannot: that
// check only ever migrates an empty database.
func TestMigrate_AddNodeExecutionIdentityPreservesExistingNodeRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.UpTo(ctx, preNodeExecutionIdentityVersion); err != nil {
		t.Fatalf("UpTo(%d): %v", preNodeExecutionIdentityVersion, err)
	}

	sessionID := "01SESSION000000000000000"
	if _, err := db.write.ExecContext(ctx,
		`INSERT INTO sessions (id, name, status, workflow, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, "s1", "up", "wf", formatTime(now), formatTime(now),
	); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.write.ExecContext(ctx,
		`INSERT INTO node_instances (session_id, node_id, task_id, scope, status, sequence, inputs_json, outputs_json, error, setup_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, "n1", "work", "session", "failed", 1, `{"a":1}`, `{"b":2}`, "boom", formatTime(now),
	); err != nil {
		t.Fatalf("seed node_instances row: %v", err)
	}
	if _, err := db.write.ExecContext(ctx,
		`INSERT INTO node_instance_layers (session_id, node_id, position, effect_id, status, inputs_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, "n1", 0, "eff1", "failed", `{"c":3}`,
	); err != nil {
		t.Fatalf("seed node_instance_layers row: %v", err)
	}

	if _, err := provider.UpByOne(ctx); err != nil {
		t.Fatalf("UpByOne (apply node-execution-identity): %v", err)
	}

	var execID, taskID, scope, status, inputsJSON, outputsJSON, errCol string
	if err := db.write.QueryRowContext(ctx,
		`SELECT id, task_id, scope, status, inputs_json, outputs_json, error FROM node_executions WHERE session_id = ? AND node_id = ?`,
		sessionID, "n1",
	).Scan(&execID, &taskID, &scope, &status, &inputsJSON, &outputsJSON, &errCol); err != nil {
		t.Fatalf("read migrated node_executions row: %v", err)
	}
	if taskID != "work" || scope != "session" || status != "failed" || inputsJSON != `{"a":1}` || outputsJSON != `{"b":2}` || errCol != "boom" {
		t.Fatalf("migrated execution = (%q,%q,%q,%q,%q,%q), want the seeded node_instances values intact", taskID, scope, status, inputsJSON, outputsJSON, errCol)
	}

	var layerEffectID, layerStatus, layerInputsJSON string
	if err := db.write.QueryRowContext(ctx,
		`SELECT effect_id, status, inputs_json FROM node_execution_layers WHERE execution_id = ? AND position = 0`,
		execID,
	).Scan(&layerEffectID, &layerStatus, &layerInputsJSON); err != nil {
		t.Fatalf("read migrated node_execution_layers row: %v", err)
	}
	if layerEffectID != "eff1" || layerStatus != "failed" || layerInputsJSON != `{"c":3}` {
		t.Fatalf("migrated layer = (%q,%q,%q), want the seeded node_instance_layers values intact", layerEffectID, layerStatus, layerInputsJSON)
	}
}

// Best-effort, not lossless -- see the migration's own Down comment.
func TestMigrate_DownRestoresNodeInstancesColumnsWithoutError(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{Name: "s4", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"n1": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Outputs: map[string]any{"b": float64(2)}},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	provider, err := goose.NewProvider(goose.DialectSQLite3, db.write, db.migrations)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}

	var taskID, scope, status, outputsJSON string
	if err := db.write.QueryRowContext(ctx,
		`SELECT task_id, scope, status, outputs_json FROM node_instances WHERE node_id = ?`, "n1",
	).Scan(&taskID, &scope, &status, &outputsJSON); err != nil {
		t.Fatalf("read restored node_instances row: %v", err)
	}
	if taskID != "work" || scope != contract.TaskScopeSession || status != contract.TaskStatusProduced || outputsJSON != `{"b":2}` {
		t.Fatalf("restored node = (%q,%q,%q,%q), want the seeded values intact", taskID, scope, status, outputsJSON)
	}
}
