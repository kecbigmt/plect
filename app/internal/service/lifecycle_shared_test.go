package service

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TestMergeTasks_ClearsNewExecutionAfterASuccessfulWrite proves a node
// persisted via mergeTasks is no longer treated as an unclaimed identity
// afterward: Up's own UpOrder() deliberately re-walks every node (see
// task.Plan.UpOrder's doc comment), so a second, redundant persistence of
// the very same in-memory object -- exactly what recreateSessionRuntime's
// own mergeTasks followed by Up's outer runNodeSetup/mergeTasks produces --
// must read as an ordinary update, not collide with the row this same call
// already inserted.
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

	// A second, redundant write of the exact same in-memory object (mirroring
	// Up's own UpOrder() re-walk after recreateSessionRuntime) must succeed as
	// an ordinary update rather than a refused conflict.
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
