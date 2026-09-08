package service

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TestMergeTasks_PreservesConfirmedExecutionIdentityAfterASuccessfulWrite
// proves a node persisted via mergeTasks carries its real, confirmed
// ExecutionID back onto the caller's own object afterward -- Up's UpOrder()
// re-walk (task.Plan.UpOrder) persists the same in-memory object again
// later in one call, and without the real id that second write would fall
// back to "whatever unreleased row exists now" instead of targeting its own
// row by exact id.
func TestMergeTasks_PreservesConfirmedExecutionIdentityAfterASuccessfulWrite(t *testing.T) {
	store := testStore(t)
	sessionName := "session1"
	now := time.Now()
	if err := store.Put(&domain.Session{Name: sessionName, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	session := &domain.Session{
		Name: sessionName,
		Nodes: map[string]*contract.TaskState{
			"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", NewExecution: true},
		},
		UpdatedAt: now,
	}
	if err := mergeTasks(store, sessionName, session); err != nil {
		t.Fatalf("mergeTasks (first write): %v", err)
	}
	if session.Nodes["a"].NewExecution || session.Nodes["a"].ExecutionID == "" {
		t.Fatalf("a after a successful write = %+v, want NewExecution cleared and a confirmed ExecutionID", session.Nodes["a"])
	}

	if err := mergeTasks(store, sessionName, session); err != nil {
		t.Fatalf("mergeTasks (redundant re-write of the same object): %v", err)
	}
}

// TestMergeTasks_RefusesAStaleReWriteAfterAnInterveningReleaseAndRebuild
// proves the confirmed ExecutionID mergeTasks copies back is actually
// enforced against a later write of the same object: an intervening writer
// releasing and recreating the node between two of this caller's own
// mergeTasks calls must make the second call fail rather than overwrite or
// resurrect the intervening writer's own fresh generation.
func TestMergeTasks_RefusesAStaleReWriteAfterAnInterveningReleaseAndRebuild(t *testing.T) {
	store := testStore(t)
	sessionName := "session1"
	now := time.Now()
	if err := store.Put(&domain.Session{Name: sessionName, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	session := &domain.Session{
		Name: sessionName,
		Nodes: map[string]*contract.TaskState{
			"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", NewExecution: true},
		},
		UpdatedAt: now,
	}
	if err := mergeTasks(store, sessionName, session); err != nil {
		t.Fatalf("mergeTasks (first write): %v", err)
	}
	staleID := session.Nodes["a"].ExecutionID

	intervening := &domain.Session{Name: sessionName, UpdatedAt: now, Nodes: map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusCleaned, TaskID: "work", ExecutionID: staleID},
	}}
	if err := mergeTasks(store, sessionName, intervening); err != nil {
		t.Fatalf("mergeTasks (intervening writer's release): %v", err)
	}
	intervening.Nodes = map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "other-work", NewExecution: true},
	}
	if err := mergeTasks(store, sessionName, intervening); err != nil {
		t.Fatalf("mergeTasks (intervening writer's rebuild): %v", err)
	}

	// session still holds its own stale copy (status produced, the id the
	// intervening writer has since released and superseded).
	if err := mergeTasks(store, sessionName, session); err == nil {
		t.Fatal("mergeTasks (stale re-write after an intervening release+rebuild): want a conflict error, got nil")
	}

	got, err := store.GetE(sessionName)
	if err != nil {
		t.Fatalf("GetE: %v", err)
	}
	if node := got.Nodes["a"]; node == nil || node.TaskID != "other-work" {
		t.Fatalf("node after the refused stale write = %+v, want the intervening writer's generation left untouched", node)
	}
}

// TestReplaceRuntimeState_PreservesConfirmedExecutionIdentityAfterASuccessfulWrite
// mirrors the mergeTasks case for replaceRuntimeState, recreateSessionRuntime's
// own write helper.
func TestReplaceRuntimeState_PreservesConfirmedExecutionIdentityAfterASuccessfulWrite(t *testing.T) {
	store := testStore(t)
	sessionName := "session1"
	now := time.Now()
	if err := store.Put(&domain.Session{Name: sessionName, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	session := &domain.Session{
		Name: sessionName,
		Nodes: map[string]*contract.TaskState{
			"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", NewExecution: true},
		},
		Tasks:     map[string]*contract.TaskState{},
		UpdatedAt: now,
	}
	if err := replaceRuntimeState(store, sessionName, session); err != nil {
		t.Fatalf("replaceRuntimeState (first write): %v", err)
	}
	if session.Nodes["a"].NewExecution || session.Nodes["a"].ExecutionID == "" {
		t.Fatalf("a after a successful write = %+v, want NewExecution cleared and a confirmed ExecutionID", session.Nodes["a"])
	}

	if err := replaceRuntimeState(store, sessionName, session); err != nil {
		t.Fatalf("replaceRuntimeState (redundant re-write of the same object): %v", err)
	}
}
