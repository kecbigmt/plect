package service

import (
	"context"
	"fmt"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// DownParams holds parameters for Down.
type DownParams struct {
	Identifier string
	Observer   task.Observer
}

// DownResult holds the outcome of Down.
type DownResult struct {
	SessionName string                         `json:"session_name"`
	Tasks       map[string]*contract.TaskState `json:"tasks,omitempty"`
}

// Down runs run-scoped cleanup (in reverse order) for the given session.
func Down(cfg *config.Config, store *state.Store, params DownParams) (*DownResult, error) {
	sessionName, session, err := resolveSession(cfg, store, params.Identifier)
	if err != nil {
		return nil, err
	}
	params.Observer = withNodeResultRecording(store, sessionName, params.Observer)
	// Running cleanup against an existing session mutates it; clamp it to the
	// active guard like the other write paths.
	if guardErr := checkSessionGuard(cfg, sessionName); guardErr != nil {
		return nil, guardErr
	}
	if guardErr := checkLifecycleRelationGuard(store, sessionName, "down"); guardErr != nil {
		return nil, guardErr
	}
	if session.Tasks == nil {
		session.Tasks = make(map[string]*contract.TaskState)
	}

	plan, err := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}

	// A single reverse-instantiation teardown over the run-scoped tasks —
	// static run nodes and run-scoped dynamic instances merged into one
	// seq-descending pass, so a static node instantiated after a dynamic
	// one is still cleaned ahead of it.
	teardown, teardownErr := unifiedTeardownList(cfg, session, plan, true)
	if teardownErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: teardownErr.Error()}
	}
	cleanupErr := task.RunCleanup(context.Background(), teardown, sessionVars(cfg, session, plan), session.Tasks, params.Observer)
	session.UpdatedAt = time.Now()
	if err := store.Put(session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	if cleanupErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: cleanupErr.Error()}
	}
	recordLifecycle(store, sessionName, "down", "run-scoped tasks cleaned")
	return &DownResult{SessionName: sessionName, Tasks: session.Tasks}, nil
}
