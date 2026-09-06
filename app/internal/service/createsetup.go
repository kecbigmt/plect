package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/dispatch"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// createWithWorkflowSetup is the workflow-setup create path:
//
//	state entry → workflow setup (acquires workspace) → cascade resolution
//	from workspace → task DAG compile → session-scoped tasks
//
// sessionName is the final id (tag already applied); resource is the
// canonical resource identifier; alias is the user's original input.
//
// The state entry is recorded before setup runs so a failed setup leaves an
// inspectable session (with the @workflow pseudo-node marked failed) that a
// later create retries and a non-force destroy can immediately release.
func createWithWorkflowSetup(cfg *config.Config, store *state.Store, params CreateParams, wf config.WorkflowFile, prov config.WorkspaceProviderConfig, sessionName, resource, alias string) (*CreateResult, error) {
	if err := validateSessionName(sessionName); err != nil {
		return nil, err
	}
	if guardErr := checkSessionGuard(cfg, sessionName); guardErr != nil {
		return nil, guardErr
	}
	params.Observer = withNodeResultRecording(store, sessionName, params.Observer)

	now := time.Now()
	var session *domain.Session
	if existing := store.Get(sessionName); existing != nil {
		if params.Population != nil && !samePopulation(existing.Population, params.Population) {
			return nil, populationCollision(sessionName, existing.Population, params.Population)
		}
		if params.Inputs != nil {
			return nil, &Error{Code: ErrInvalidInput, Message: inputsOnExistingSessionMessage()}
		}
		if params.Workflow != "" && params.Workflow != existing.Workflow {
			return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("--workflow %q does not match the session's frozen workflow %q; destroy and recreate to switch", params.Workflow, existing.Workflow)}
		}
		if existing.Workflow != "" && existing.Workflow != wf.Address {
			return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("resource dispatches to workflow %q but session %q is frozen to %q; destroy and recreate to switch", wf.Address, sessionName, existing.Workflow)}
		}
		session = existing
		if session.Tasks == nil {
			session.Tasks = make(map[string]*contract.TaskState)
		}
	} else {
		parentSession, parentErr := resolveParentSession(store, sessionName, params.ParentSession)
		if parentErr != nil {
			return nil, parentErr
		}
		// Session inputs are validated against the trusted-layer schema;
		// workspace-dir overlays can add nodes but not tighten the input
		// contract retroactively (the workspace doesn't exist at validation
		// time).
		input, validateErr := resolveSessionInputs(cfg, "", wf.Address, params.Inputs)
		if validateErr != nil {
			return nil, validateErr
		}
		session = &domain.Session{
			Name: sessionName,
			// The address, not the id: the id names the session and cannot say
			// which declaration produced it, so a plan reloaded later would
			// look for a workflow that answers to something else.
			ParentSession: parentSession,
			Workflow:      wf.Address,
			Population:    params.Population,
			Inputs:        input,
			Tasks:         make(map[string]*contract.TaskState),
			CreatedAt:     now,
		}
		// Seed the dispatcher's read cursor at this fresh session's empty log tail
		// so the initial task instruction, appended below during create, is
		// delivered. The dispatcher only starts once the run scope comes up (after
		// create returns), by which point its own first-start seed would land past
		// the instruction and drop it.
		dispatch.SeedCursor(eventlog.NewStore(store.Dir()), sessionName)
	}
	session.ResourceID = resource
	session.Alias = alias
	session.UpdatedAt = now

	// Record the session before setup so partial failures stay visible.
	if err := store.Put(session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}

	reused := false
	if st, ok := session.Tasks[contract.WorkflowPseudoNodeID]; ok && st != nil && st.Status == contract.TaskStatusProduced {
		reused = true
	}

	provInputs, provInputsErr := resolveWorkspaceProviderInputs(prov, wf)
	if provInputsErr != nil {
		return nil, &Error{Code: ErrInvalidInput, Message: provInputsErr.Error()}
	}
	vars := effect.WorkflowHookVars{
		ResourceID:        resource,
		SessionName:       sessionName,
		WorkspaceDirsRoot: cfg.WorkspaceDirsRoot,
		SessionInputs:     session.Inputs,
		Inputs:            provInputs,
		Plugins:           cfg.Plugins,
		SourcePath:        prov.SourcePath,
	}
	outputs, setupErr := task.RunWorkflowSetup(prov, vars, session.Tasks, params.Observer)
	session.UpdatedAt = time.Now()
	if outputs != nil {
		if workspaceDir, ok := outputs[contract.OutputKeyWorkspaceDir].(string); ok {
			// The session's own workspace-directory field is the one every
			// consumer (cd/attach/ls/web UI/hooks) reads, so mirror it here.
			session.WorkspaceDirPath = workspaceDir
		}
		if branch, ok := outputs["branch"].(string); ok && branch != "" {
			session.Branch = branch
		}
	}
	if err := store.Put(session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	if setupErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: setupErr.Error()}
	}

	// The workspace now exists: resolve the full cascade (incl. overlays
	// above and the node-only layer inside the workspace dir) and run
	// session tasks.
	plan, err := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	tasksErr := task.RunSetup(context.Background(), plan.Session, sessionVars(cfg, session, plan), session.Tasks, params.Observer)
	session.UpdatedAt = time.Now()
	// A session node (the initial_task dispatcher) can shell out to a nested
	// `plect task setup` subprocess that writes its instance straight to disk.
	// Overlay our in-memory task entries onto the freshly-read session instead
	// of a blind Put so that nested write (e.g. the `initial` task) survives.
	if err := mergeTasks(store, sessionName, session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	if tasksErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: tasksErr.Error()}
	}
	if refreshed := store.Get(sessionName); refreshed != nil {
		session = refreshed
	}

	// Binding implies delivery for the session's own resource too, not just
	// a dynamic task setup's own bound one.
	if _, errMsg := wireDeliveryOnSetup(cfg, store, sessionName, resource); errMsg != "" {
		slog.Warn("resource delivery wiring failed at session create", "session", sessionName, "resource", resource, "error", errMsg)
	}

	// Record lifecycle.created on the first successful create (idempotent across
	// retries of a partial failure and re-runs of an already-created session).
	recordSessionCreated(store, sessionName)

	return &CreateResult{
		SessionName:        sessionName,
		WorkspaceDirPath:   session.WorkspaceDirPath,
		Branch:             session.Branch,
		ReusedWorkspaceDir: reused,
		Tasks:              session.Tasks,
	}, nil
}
