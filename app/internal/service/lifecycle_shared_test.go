package service

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TestMergeTasks_ClearsNewExecutionAfterASuccessfulWrite proves a node
// persisted via mergeTasks is no longer an unclaimed identity afterward, so
// Up's UpOrder() re-walk (task.Plan.UpOrder) can redundantly persist the
// same object again without colliding with its own prior insert.
func TestMergeTasks_ClearsNewExecutionAfterASuccessfulWrite(t *testing.T) {
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
	if session.Nodes["a"].NewExecution {
		t.Fatal("NewExecution still true after a successful write; a redundant re-persist of this same object would misread as a concurrent writer's row")
	}

	if err := mergeTasks(store, sessionName, session); err != nil {
		t.Fatalf("mergeTasks (redundant re-write of the same object): %v", err)
	}
}

// TestReplaceRuntimeState_ClearsNewExecutionAfterASuccessfulWrite mirrors
// the mergeTasks case for replaceRuntimeState, recreateSessionRuntime's own
// write helper.
func TestReplaceRuntimeState_ClearsNewExecutionAfterASuccessfulWrite(t *testing.T) {
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
	if session.Nodes["a"].NewExecution {
		t.Fatal("NewExecution still true after a successful write")
	}

	if err := replaceRuntimeState(store, sessionName, session); err != nil {
		t.Fatalf("replaceRuntimeState (redundant re-write of the same object): %v", err)
	}
}
