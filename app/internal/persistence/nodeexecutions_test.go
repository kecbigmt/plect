package persistence

import (
	"context"
	"encoding/json"
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

// TestPutSession_NodeExecutionRetainedFactsRoundTrip proves ExecutionDir,
// Cleanup (the retained cleanup contract), and PluginRef survive a
// PutSession/GetSession round trip -- persistence stores Cleanup opaquely
// (json.RawMessage) and must not alter its bytes.
func TestPutSession_NodeExecutionRetainedFactsRoundTrip(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	cleanup := json.RawMessage(`{"action":{"Type":"shell","Script":"true"},"from":{"IsPlugin":true,"Alias":"gh"}}`)
	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {
			Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work",
			ExecutionDir: "/tmp/workdir", Cleanup: cleanup, PluginRef: "gh",
		},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	node := got.Nodes["a"]
	if node == nil {
		t.Fatal("node missing")
	}
	if node.ExecutionDir != "/tmp/workdir" {
		t.Errorf("ExecutionDir = %q, want %q", node.ExecutionDir, "/tmp/workdir")
	}
	if node.PluginRef != "gh" {
		t.Errorf("PluginRef = %q, want %q", node.PluginRef, "gh")
	}
	if string(node.Cleanup) != string(cleanup) {
		t.Errorf("Cleanup = %s, want %s", node.Cleanup, cleanup)
	}
}

// TestPutSession_NodeExecutionLayerCleanupRoundTrips proves a nested node's
// per-layer retained cleanup contract (LayerState.Cleanup) survives a
// PutSession/GetSession round trip, distinct per layer.
func TestPutSession_NodeExecutionLayerCleanupRoundTrips(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	outerCleanup := json.RawMessage(`{"effect_id":"outer","cleanup":{"Type":"shell","Script":"outer-cleanup"},"from":{}}`)
	innerCleanup := json.RawMessage(`{"effect_id":"inner","cleanup":{"Type":"shell","Script":"inner-cleanup"},"from":{}}`)
	session := &domain.Session{Name: "s1", CreatedAt: now, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {
			Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work",
			Layers: []contract.LayerState{
				{EffectID: "outer", Status: contract.TaskStatusProduced, Cleanup: outerCleanup},
				{EffectID: "inner", Status: contract.TaskStatusProduced, Cleanup: innerCleanup},
			},
		},
	}}
	if err := db.PutSession(ctx, session); err != nil {
		t.Fatalf("PutSession: %v", err)
	}

	got, err := db.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	node := got.Nodes["a"]
	if node == nil || len(node.Layers) != 2 {
		t.Fatalf("node.Layers = %+v, want 2 layers", node)
	}
	if string(node.Layers[0].Cleanup) != string(outerCleanup) {
		t.Errorf("layer 0 Cleanup = %s, want %s", node.Layers[0].Cleanup, outerCleanup)
	}
	if string(node.Layers[1].Cleanup) != string(innerCleanup) {
		t.Errorf("layer 1 Cleanup = %s, want %s", node.Layers[1].Cleanup, innerCleanup)
	}
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
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "other-work"},
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
