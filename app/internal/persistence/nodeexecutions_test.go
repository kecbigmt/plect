package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/persistence/sqlcgen"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// nodeExecutionIDForTest reads a node_executions row's own id column
// directly, bypassing the persistence-layer read path (which never surfaces
// it), so a test can prove the id itself is stable or fresh across writes.
// It returns the row with the highest sequence when more than one exists.
func nodeExecutionIDForTest(t *testing.T, db *DB, sessionName, nodeID string) string {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(db.write)
	sessionID, err := q.SessionIDByLiveName(ctx, sessionName)
	if err != nil {
		t.Fatalf("SessionIDByLiveName(%q): %v", sessionName, err)
	}
	rows, err := q.ListCurrentNodeExecutions(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListCurrentNodeExecutions(%q): %v", sessionName, err)
	}
	for _, row := range rows {
		if row.NodeID == nodeID {
			return row.ID
		}
	}
	t.Fatalf("no current node_executions row for %q/%q", sessionName, nodeID)
	return ""
}

// countNodeExecutionsForTest returns how many node_executions rows exist for
// nodeID across every generation, not just the current one.
func countNodeExecutionsForTest(t *testing.T, db *DB, sessionName, nodeID string) int {
	t.Helper()
	ctx := context.Background()
	q := sqlcgen.New(db.write)
	sessionID, err := q.SessionIDByLiveName(ctx, sessionName)
	if err != nil {
		t.Fatalf("SessionIDByLiveName(%q): %v", sessionName, err)
	}
	var count int
	if err := db.write.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_executions WHERE session_id = ? AND node_id = ?`, sessionID, nodeID,
	).Scan(&count); err != nil {
		t.Fatalf("count node_executions(%q/%q): %v", sessionName, nodeID, err)
	}
	return count
}

// TestPutSession_NodeExecutionIDStableAcrossOrdinaryUpdate mirrors
// TestPutSession_DynamicInstanceIDStableAcrossOrdinaryUpdate for the node
// side: an ordinary re-Put of the same declared node (same task_id/scope)
// updates the existing unreleased execution in place rather than minting a
// new one.
func TestPutSession_NodeExecutionIDStableAcrossOrdinaryUpdate(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	firstID := nodeExecutionIDForTest(t, db, "s1", "a")

	updated := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Outputs: map[string]any{"x": float64(1)}},
	}}
	if err := db.PutSession(ctx, updated); err != nil {
		t.Fatalf("PutSession (update): %v", err)
	}
	secondID := nodeExecutionIDForTest(t, db, "s1", "a")

	if firstID != secondID {
		t.Fatalf("execution id changed across an ordinary update: first = %q, second = %q", firstID, secondID)
	}
	if countNodeExecutionsForTest(t, db, "s1", "a") != 1 {
		t.Fatalf("ordinary update minted an extra execution row")
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if node := got.Nodes["a"]; node == nil || node.Outputs["x"] != float64(1) {
		t.Fatalf("node after update = %+v, want outputs.x = 1", node)
	}
}

// TestPutSession_NodeExecutionMintsFreshIdentityAfterRelease proves a node
// that goes produced -> cleaned -> produced again (a release followed by a
// genuinely new setup attempt) gets a fresh execution id for the new
// attempt, while the released generation remains in the table as retained
// history rather than being overwritten.
func TestPutSession_NodeExecutionMintsFreshIdentityAfterRelease(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	firstID := nodeExecutionIDForTest(t, db, "s1", "a")

	released := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusCleaned, TaskID: "work"},
	}}
	if err := db.PutSession(ctx, released); err != nil {
		t.Fatalf("PutSession (release): %v", err)
	}

	recreated := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "other-work", NewExecution: true},
	}}
	if err := db.PutSession(ctx, recreated); err != nil {
		t.Fatalf("PutSession (recreate): %v", err)
	}
	secondID := nodeExecutionIDForTest(t, db, "s1", "a")

	if firstID == secondID {
		t.Fatalf("a new setup attempt after release reused the released execution's id %q", firstID)
	}
	if countNodeExecutionsForTest(t, db, "s1", "a") != 2 {
		t.Fatalf("release-then-recreate should retain both generations as history, got %d rows", countNodeExecutionsForTest(t, db, "s1", "a"))
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if node := got.Nodes["a"]; node == nil || node.TaskID != "other-work" || node.Status != contract.TaskStatusProduced {
		t.Fatalf("node after recreate = %+v, want the new generation's fields", node)
	}
}

// TestPutSession_NewExecutionRefusesAConcurrentWritersUnreleasedRow proves a
// write claiming NewExecution is refused rather than silently overwriting a
// different writer's unreleased row for the same node.
func TestPutSession_NewExecutionRefusesAConcurrentWritersUnreleasedRow(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	winner := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Outputs: map[string]any{"from": "writer-a"}, NewExecution: true},
	}}
	if err := db.PutSession(ctx, winner); err != nil {
		t.Fatalf("PutSession (writer A): %v", err)
	}
	winnerID := nodeExecutionIDForTest(t, db, "s1", "a")

	loser := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Outputs: map[string]any{"from": "writer-b"}, NewExecution: true},
	}}
	if err := db.PutSession(ctx, loser); err == nil {
		t.Fatal("PutSession (writer B, concurrent new setup): want a conflict error, got nil")
	}

	if got := countNodeExecutionsForTest(t, db, "s1", "a"); got != 1 {
		t.Fatalf("node_executions rows for %q/%q = %d, want 1 (the losing writer must not mint a second row)", "s1", "a", got)
	}
	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	node := got.Nodes["a"]
	if node == nil || node.Outputs["from"] != "writer-a" || node.ExecutionID != winnerID {
		t.Fatalf("node after the refused race = %+v, want writer A's execution left untouched", node)
	}
}

// TestPutSession_NewExecutionInsertsFreshRowWhenNodeIsGenuinelyNew proves
// NewExecution still inserts normally when the node has no prior row.
func TestPutSession_NewExecutionInsertsFreshRowWhenNodeIsGenuinelyNew(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", NewExecution: true},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	if got := countNodeExecutionsForTest(t, db, "s1", "a"); got != 1 {
		t.Fatalf("node_executions rows for %q/%q = %d, want 1", "s1", "a", got)
	}
}

// TestPutSession_RestartAfterReleaseThenNewSetupMintsFreshGeneration proves
// a release flushed durably on its own (see task.ReleaseObserver) survives a
// crash landing right after it, and a later setup still mints a fresh
// generation rather than colliding with or resurrecting the released one.
func TestPutSession_RestartAfterReleaseThenNewSetupMintsFreshGeneration(t *testing.T) {
	dir := t.TempDir()
	path := PathIn(dir)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	releasedID := nodeExecutionIDForTest(t, db, "s1", "a")

	if err := db.PutSession(ctx, &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusCleaned, TaskID: "work", ExecutionID: releasedID},
	}}); err != nil {
		t.Fatalf("PutSession (release): %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted, err := Open(path)
	if err != nil {
		t.Fatalf("Open (restart): %v", err)
	}
	defer restarted.Close()
	if err := restarted.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (restart): %v", err)
	}

	got, err := restarted.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession (after restart): %v", err)
	}
	node := got.Nodes["a"]
	if node == nil || node.Status != contract.TaskStatusCleaned || node.ExecutionID != releasedID {
		t.Fatalf("node after restart = %+v, want the release durably recorded on its own", node)
	}

	if err := restarted.PutSession(ctx, &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", NewExecution: true},
	}}); err != nil {
		t.Fatalf("PutSession (setup after restart): %v", err)
	}
	if got := countNodeExecutionsForTest(t, restarted, "s1", "a"); got != 2 {
		t.Fatalf("node_executions rows after restart + fresh setup = %d, want 2 (both generations retained)", got)
	}
	newID := nodeExecutionIDForTest(t, restarted, "s1", "a")
	if newID == releasedID {
		t.Fatalf("fresh setup after restart reused the released execution's id %q", releasedID)
	}
}

// TestPutSession_NodeDependencyRoundTripsAndOrdersWithinOneWrite proves
// TaskState.DependsOn round-trips through node_execution_dependencies even
// when the dependency and its dependent are written in the same PutSession
// call -- persistence must order its own internal writes so the dependency
// row exists before the dependent's edge is resolved against it (map
// iteration order is otherwise random).
func TestPutSession_NodeDependencyRoundTripsAndOrdersWithinOneWrite(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "producer"},
		"b": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "consumer", DependsOn: []string{"a"}},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	b := got.Nodes["b"]
	if b == nil || len(b.DependsOn) != 1 || b.DependsOn[0] != "a" {
		t.Fatalf("node %q DependsOn = %v, want [%q]", "b", b, "a")
	}
	if a := got.Nodes["a"]; a == nil || len(a.DependsOn) != 0 {
		t.Fatalf("node %q DependsOn = %v, want none", "a", a)
	}
}

// TestPutSession_NodeDependencyOnAnAlreadyReleasedNodeIsDropped proves a
// recorded dependency on a node with no current unreleased execution (never
// set up, or already cleaned) is simply omitted rather than erroring --
// there is nothing left to order this execution's release against.
func TestPutSession_NodeDependencyOnAnAlreadyReleasedNodeIsDropped(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"b": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "consumer", DependsOn: []string{"nonexistent"}},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if b := got.Nodes["b"]; b == nil || len(b.DependsOn) != 0 {
		t.Fatalf("node %q DependsOn = %v, want none (no unreleased execution to depend on)", "b", b)
	}
}

// TestPutSession_NodeDependencyEdgeSurvivesDependencyNodeGoingUnreleased
// proves a dependency edge recorded at a node's own setup time keeps
// resolving to the same prerequisite node id even across later writes that
// change unrelated fields -- release ordering (service.unifiedTeardownList)
// reads TaskState.DependsOn directly off the loaded session, so this is the
// persistence-level guarantee that read remains correct on.
func TestPutSession_NodeDependencyEdgeSurvivesDependencyNodeGoingUnreleased(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "producer"},
		"b": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "consumer", DependsOn: []string{"a"}},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	// "a" transitions to failed (still unreleased) on its own, unrelated write.
	session.Nodes = map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusFailed, TaskID: "producer", Error: "boom"},
		"b": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "consumer", DependsOn: []string{"a"}},
	}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession (2nd): %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if b := got.Nodes["b"]; b == nil || len(b.DependsOn) != 1 || b.DependsOn[0] != "a" {
		t.Fatalf("node %q DependsOn = %v, want [%q] to survive %q's own status change", "b", b, "a", "a")
	}
}

// TestPutSession_RefusesUpdateAgainstAnExecutionSupersededByAnotherWriter
// proves a write that read one unreleased generation, then tries to update
// it in place after a different writer released and recreated that node in
// between, is refused rather than silently overwriting the new generation's
// own release recipe with the stale writer's data.
func TestPutSession_RefusesUpdateAgainstAnExecutionSupersededByAnotherWriter(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	staleID := nodeExecutionIDForTest(t, db, "s1", "a")

	// A different writer releases "a" and starts a brand new attempt.
	if err := db.PutSession(ctx, &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusCleaned, TaskID: "work"},
	}}); err != nil {
		t.Fatalf("PutSession (release): %v", err)
	}
	if err := db.PutSession(ctx, &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "other-work"},
	}}); err != nil {
		t.Fatalf("PutSession (recreate): %v", err)
	}

	// The stale writer, unaware of the release+recreate, tries to update the
	// generation it originally read.
	staleWrite := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", ExecutionID: staleID},
	}}
	if err := db.PutSession(ctx, staleWrite); err == nil {
		t.Fatal("PutSession (stale update): want a conflict error, got nil")
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if node := got.Nodes["a"]; node == nil || node.TaskID != "other-work" {
		t.Fatalf("node after refused stale write = %+v, want the newer generation left untouched", node)
	}
}

// TestPutSession_RefusesUpdateAgainstAnExecutionAlreadyReleasedByAnotherWriter
// covers the symmetric case: the generation the stale writer read has been
// released with nothing new set up in its place yet, so no unreleased
// execution exists at all for the node.
func TestPutSession_RefusesUpdateAgainstAnExecutionAlreadyReleasedByAnotherWriter(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	staleID := nodeExecutionIDForTest(t, db, "s1", "a")

	if err := db.PutSession(ctx, &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusCleaned, TaskID: "work"},
	}}); err != nil {
		t.Fatalf("PutSession (release): %v", err)
	}

	staleWrite := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusFailed, TaskID: "work", ExecutionID: staleID, Error: "boom"},
	}}
	if err := db.PutSession(ctx, staleWrite); err == nil {
		t.Fatal("PutSession (stale update): want a conflict error, got nil")
	}
}

func TestPutSession_RewritingAnAlreadyCleanedStateDoesNotDuplicateTheRow(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work"},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}
	id := nodeExecutionIDForTest(t, db, "s1", "a")

	cleaned := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusCleaned, TaskID: "work", ExecutionID: id},
	}}
	if err := db.PutSession(ctx, cleaned); err != nil {
		t.Fatalf("PutSession (cleaned): %v", err)
	}

	if err := db.PutSession(ctx, cleaned); err != nil {
		t.Fatalf("PutSession (re-persist cleaned): %v", err)
	}

	if got := countNodeExecutionsForTest(t, db, "s1", "a"); got != 1 {
		t.Fatalf("node_executions rows for %q/%q = %d, want 1 (no duplicate cleaned row)", "s1", "a", got)
	}
}

// TestPutSession_NodeDependencyEdgeDoesNotLeakAcrossReleaseAndRecreate proves
// a released generation's own retained dependency row does not leak into a
// later recreation's DependsOn, checked through both GetSession and
// AllSessions.
func TestPutSession_NodeDependencyEdgeDoesNotLeakAcrossReleaseAndRecreate(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"dep": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "producer"},
		"a":   {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", DependsOn: []string{"dep"}},
	}}
	if err := db.PutSession(ctx, seed); err != nil {
		t.Fatalf("PutSession (seed): %v", err)
	}

	// A release write carries over DependsOn: RunCleanup mutates the loaded
	// TaskState's Status/CleanedAt in place and never clears it.
	released := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"dep": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "producer"},
		"a":   {Scope: contract.TaskScopeSession, Status: contract.TaskStatusCleaned, TaskID: "work", DependsOn: []string{"dep"}},
	}}
	if err := db.PutSession(ctx, released); err != nil {
		t.Fatalf("PutSession (release): %v", err)
	}

	recreated := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"dep": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "producer"},
		"a":   {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "other-work"},
	}}
	if err := db.PutSession(ctx, recreated); err != nil {
		t.Fatalf("PutSession (recreate): %v", err)
	}
	if countNodeExecutionsForTest(t, db, "s1", "a") != 2 {
		t.Fatalf("release-then-recreate should retain both generations as history, got %d rows", countNodeExecutionsForTest(t, db, "s1", "a"))
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if a := got.Nodes["a"]; a == nil || len(a.DependsOn) != 0 {
		t.Fatalf("GetSession node %q DependsOn = %v, want none (the new generation declared no dependencies)", "a", a)
	}

	all, err := db.AllSessions(ctx)
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if a := all["s1"].Nodes["a"]; a == nil || len(a.DependsOn) != 0 {
		t.Fatalf("AllSessions node %q DependsOn = %v, want none", "a", a)
	}
}
