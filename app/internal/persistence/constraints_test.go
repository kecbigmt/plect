package persistence

import (
	"context"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// seedBareSessionForTest inserts a minimal, live sessions row and returns
// its id, satisfying a later insert's session_id foreign key.
func seedBareSessionForTest(t *testing.T, db *DB, name string) string {
	t.Helper()
	q := sqlcgen.New(db.write)
	id := newULID()
	if err := q.InsertSession(context.Background(), sqlcgen.InsertSessionParams{
		ID: id, Name: name, Status: contract.SessionStatusDown, CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed session %q: %v", name, err)
	}
	return id
}

// taskInstanceIDForTest reads a task_instances row's own id column directly,
// bypassing the persistence-layer read path (which never surfaces it), so a
// test can prove the id itself is stable or fresh across writes.
func taskInstanceIDForTest(t *testing.T, db *DB, sessionName, instanceName string) string {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(db.write)
	sessionID, err := q.SessionIDByLiveName(ctx, sessionName)
	if err != nil {
		t.Fatalf("SessionIDByLiveName(%q): %v", sessionName, err)
	}
	rows, err := q.ListTaskInstances(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListTaskInstances(%q): %v", sessionName, err)
	}
	for _, row := range rows {
		if row.InstanceName == instanceName {
			return row.ID
		}
	}
	t.Fatalf("no task_instances row for %q/%q", sessionName, instanceName)
	return ""
}

// TestSchema_RejectsOutOfSetScopeAndStatus proves the CHECK IN constraints
// on the closed-set columns copied from contracts/state's own constants
// (TaskScopeSession/TaskScopeRun, TaskStatusProduced/TaskStatusFailed/
// TaskStatusCleaned): an out-of-set value is rejected at insert, on both
// node_instances and task_instances.
func TestSchema_RejectsOutOfSetScopeAndStatus(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	q := sqlcgen.New(db.write)
	sessionID := seedBareSessionForTest(t, db, "s1")

	if err := q.InsertNodeInstance(ctx, sqlcgen.InsertNodeInstanceParams{
		SessionID: sessionID, NodeID: "n1", Scope: "bogus", Status: "produced",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("InsertNodeInstance with bogus scope: err = %v, want a CHECK constraint failure", err)
	}
	if err := q.InsertNodeInstance(ctx, sqlcgen.InsertNodeInstanceParams{
		SessionID: sessionID, NodeID: "n1", Scope: "session", Status: "bogus",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("InsertNodeInstance with bogus status: err = %v, want a CHECK constraint failure", err)
	}

	if _, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID: newULID(), SessionID: sessionID, InstanceName: "i1", Scope: "bogus", Status: "produced",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("UpsertTaskInstance with bogus scope: err = %v, want a CHECK constraint failure", err)
	}
	if _, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID: newULID(), SessionID: sessionID, InstanceName: "i1", Scope: "session", Status: "bogus",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("UpsertTaskInstance with bogus status: err = %v, want a CHECK constraint failure", err)
	}
}

// TestSchema_RejectsOutOfSetJudgeActionAndRelation mirrors the above for
// task_done_when_judges.action (task.JudgeActionApprove/RequestChanges) and
// .relation (domain.SessionRelation's seven values, plus the unset "").
func TestSchema_RejectsOutOfSetJudgeActionAndRelation(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	q := sqlcgen.New(db.write)
	sessionID := seedBareSessionForTest(t, db, "s1")

	if _, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID: "task1", SessionID: sessionID, InstanceName: "i1", Scope: "session", Status: "produced",
	}); err != nil {
		t.Fatalf("seed task instance: %v", err)
	}

	if err := q.InsertTaskDoneWhenJudge(ctx, sqlcgen.InsertTaskDoneWhenJudgeParams{
		TaskInstanceID: "task1", LeafID: "leaf1", Action: "bogus", Relation: "sibling", CreatedAt: "2024-01-01T00:00:00Z",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("InsertTaskDoneWhenJudge with bogus action: err = %v, want a CHECK constraint failure", err)
	}
	if err := q.InsertTaskDoneWhenJudge(ctx, sqlcgen.InsertTaskDoneWhenJudgeParams{
		TaskInstanceID: "task1", LeafID: "leaf1", Action: "approve", Relation: "bogus", CreatedAt: "2024-01-01T00:00:00Z",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("InsertTaskDoneWhenJudge with bogus relation: err = %v, want a CHECK constraint failure", err)
	}
}

// TestSchema_RejectsOutOfSetBooleanColumns proves the BOOLEAN CHECK (col IN
// (0, 1)) constraints on population_members.tombstoned/pending_up: a value
// outside {0, 1} is rejected at insert.
func TestSchema_RejectsOutOfSetBooleanColumns(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if _, err := db.write.ExecContext(ctx, "INSERT INTO populations (workflow, name) VALUES ('wf', 'pop')"); err != nil {
		t.Fatalf("seed population: %v", err)
	}

	if _, err := db.write.ExecContext(ctx,
		"INSERT INTO population_members (workflow, name, resource_id, tombstoned) VALUES ('wf', 'pop', 'r1', 2)"); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("insert with tombstoned=2: err = %v, want a CHECK constraint failure", err)
	}
	if _, err := db.write.ExecContext(ctx,
		"INSERT INTO population_members (workflow, name, resource_id, pending_up) VALUES ('wf', 'pop', 'r1', -1)"); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("insert with pending_up=-1: err = %v, want a CHECK constraint failure", err)
	}
}

func TestSchema_RejectsUpReservationsWithBothOrNeitherParentShape(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if _, err := db.write.ExecContext(ctx,
		"INSERT INTO up_reservations (child_session_name, parent_session_name, virtual_root, pid, reserved_at) VALUES ('c1', 'parent1', 1, 1, '2024-01-01T00:00:00.000000000Z')"); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("insert with both parent_session_name and virtual_root=1: err = %v, want a CHECK constraint failure", err)
	}
	if _, err := db.write.ExecContext(ctx,
		"INSERT INTO up_reservations (child_session_name, parent_session_name, virtual_root, pid, reserved_at) VALUES ('c2', NULL, 0, 1, '2024-01-01T00:00:00.000000000Z')"); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("insert with neither parent_session_name nor virtual_root: err = %v, want a CHECK constraint failure", err)
	}
}

// TestSchema_RejectsTwoLiveSessionsWithSameName proves the partial unique
// index sessions_live_name: two rows may share a name only if at most one
// is live.
func TestSchema_RejectsTwoLiveSessionsWithSameName(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	q := sqlcgen.New(db.write)

	if err := q.InsertSession(ctx, sqlcgen.InsertSessionParams{
		ID: "id1", Name: "dup", Status: contract.SessionStatusDown, CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed first live session: %v", err)
	}
	if _, err := db.write.ExecContext(ctx,
		"INSERT INTO sessions (id, name, status, workflow, created_at, updated_at) VALUES ('id2', 'dup', 'down', '', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')"); err == nil {
		t.Fatal("insert of a second live row with the same name succeeded, want a unique-index failure")
	}

	// A destroyed row does not block a second live row with the same name.
	if _, err := db.write.ExecContext(ctx,
		"INSERT INTO sessions (id, name, status, destroyed_at, workflow, created_at, updated_at) VALUES ('id3', 'dup', 'destroyed', '2024-01-01T00:00:00Z', '', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')"); err != nil {
		t.Fatalf("insert of a destroyed row with a name already live: %v, want success", err)
	}
}
