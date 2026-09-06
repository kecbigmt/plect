package persistence

import (
	"context"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
)

// seedBareSessionForTest inserts a minimal sessions row so a
// node_instances/task_instances insert in the same test satisfies its
// session_name foreign key and fails (or succeeds) only for the reason the
// test is actually checking.
func seedBareSessionForTest(t *testing.T, db *DB, name string) {
	t.Helper()
	q := sqlcgen.New(db.write)
	if err := q.UpsertSession(context.Background(), sqlcgen.UpsertSessionParams{
		Name: name, CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-01-01T00:00:00Z", RecordJson: "{}",
	}); err != nil {
		t.Fatalf("seed session %q: %v", name, err)
	}
}

// taskInstanceIDForTest reads a task_instances row's own id column directly,
// bypassing the persistence-layer read path (which never surfaces it), so a
// test can prove the id itself is stable or fresh across writes.
func taskInstanceIDForTest(t *testing.T, db *DB, sessionName, instanceName string) string {
	t.Helper()
	rows, err := sqlcgen.New(db.write).ListTaskInstances(context.Background(), sessionName)
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
	seedBareSessionForTest(t, db, "s1")

	if err := q.InsertNodeInstance(ctx, sqlcgen.InsertNodeInstanceParams{
		SessionName: "s1", NodeID: "n1", Scope: "bogus", Status: "produced", RecordJson: "{}",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("InsertNodeInstance with bogus scope: err = %v, want a CHECK constraint failure", err)
	}
	if err := q.InsertNodeInstance(ctx, sqlcgen.InsertNodeInstanceParams{
		SessionName: "s1", NodeID: "n1", Scope: "session", Status: "bogus", RecordJson: "{}",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("InsertNodeInstance with bogus status: err = %v, want a CHECK constraint failure", err)
	}

	if _, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID: newULID(), SessionName: "s1", InstanceName: "i1", Scope: "bogus", Status: "produced", RecordJson: "{}",
	}); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("UpsertTaskInstance with bogus scope: err = %v, want a CHECK constraint failure", err)
	}
	if _, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID: newULID(), SessionName: "s1", InstanceName: "i1", Scope: "session", Status: "bogus", RecordJson: "{}",
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
	seedBareSessionForTest(t, db, "s1")

	if _, err := q.UpsertTaskInstance(ctx, sqlcgen.UpsertTaskInstanceParams{
		ID: "task1", SessionName: "s1", InstanceName: "i1", Scope: "session", Status: "produced", RecordJson: "{}",
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
