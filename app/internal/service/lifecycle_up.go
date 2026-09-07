package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// UpParams holds parameters for Up.
type UpParams struct {
	Identifier string // URL or session name
	// Tag derives a tagged session name from a URL identifier. Only valid
	// when Identifier is a URL; combining Tag with a bare session name
	// returns ErrInvalidTag so misuse surfaces immediately rather than
	// silently ignoring the flag.
	Tag           string
	Workflow      string         // forwarded to auto-create; rejected when state already exists
	Inputs        map[string]any // forwarded to auto-create; rejected when state already exists
	ParentSession string         // forwarded to auto-create; empty falls back to PLECT_SESSION_NAME when it exists and is not self.
	Observer      task.Observer
	Population    *contract.PopulationProvenance
	// ForceRecreate rebuilds the runtime for an existing session while
	// preserving durable identity and event log state.
	ForceRecreate bool
}

// UpResult holds the outcome of Up.
type UpResult struct {
	SessionName string                         `json:"session_name"`
	Tasks       map[string]*contract.TaskState `json:"tasks,omitempty"`
}

// Up runs run-scoped tasks for the given session.
//
// docker compose up-style auto-create: if the identifier is a URL, Create
// is invoked internally before run-scoped setup whenever the state entry
// is absent OR any declared session-scoped task has not reached
// "produced". Since Create is idempotent (already-produced session
// tasks are skipped), this also recovers partial-create state without
// the user having to remember a separate command. A bare session name
// without a state entry still errors out — Create needs URL information
// to resolve session/branch, so the asymmetry is intentional.
func Up(cfg *config.Config, store *state.Store, params UpParams) (*UpResult, error) {
	identifier := params.Identifier
	forceRecreateExisting := false
	disp, matched, dispErr := dispatchResource(cfg, params.Workflow, params.Identifier)
	if dispErr != nil {
		// Ambiguous resolver match / invalid resolver / explicit --workflow
		// mismatch must fail here exactly as Create would — falling through
		// to the legacy path would let `plect up` silently disagree with
		// auto-create for the same resource.
		if svcErr, ok := dispErr.(*Error); ok {
			return nil, svcErr
		}
		return nil, &Error{Code: ErrExecutionFailed, Message: dispErr.Error()}
	}
	if matched {
		// Resolver dispatch: the identifier is a resource id. Mirror the URL
		// auto-create semantics below with the resolver-derived session name.
		// The effective tag (explicit --tag or the workflow-id default) is
		// resolved here and forwarded to Create so up and create converge on
		// the same tagged session name.
		tag, tagErr := effectiveTag(params.Tag, disp.Workflow.ID)
		if tagErr != nil {
			return nil, tagErr
		}
		sessionName := disp.Name + "+" + tag
		existing, err := store.GetE(sessionName)
		if err != nil {
			return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("read session %q state: %v", sessionName, err)}
		}
		if existing != nil {
			switch {
			case params.Population != nil && !samePopulation(existing.Population, params.Population):
				return nil, populationCollision(sessionName, existing.Population, params.Population)
			case params.Population == nil && existing.Population != nil && params.ParentSession != "":
				return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("session %q is owned by workflow %q population %q and cannot be adopted by another lifecycle authority", sessionName, existing.Population.Workflow, existing.Population.Name)}
			}
		}
		forceRecreateExisting = existing != nil
		if params.Inputs != nil && existing != nil {
			return nil, &Error{Code: ErrInvalidInput, Message: inputsOnExistingSessionMessage()}
		}
		if params.Workflow != "" && existing != nil && params.Workflow != existing.Workflow {
			return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("--workflow %q does not match the session's frozen workflow %q", params.Workflow, existing.Workflow)}
		}
		// Before Create: a rejected new child must leave no state entry.
		// ForceRecreate excludes even an up target, since it tears the
		// child down and rebuilds — the slot must stay reserved through that.
		targetAlreadyUp := existing != nil && sessionRunState(cfg, existing) == domain.RunUp && !params.ForceRecreate
		parentSessionName := ""
		if existing != nil {
			parentSessionName = existing.ParentSession
		} else {
			resolved, parentErr := resolveParentSession(store, sessionName, params.ParentSession)
			if parentErr != nil {
				return nil, parentErr
			}
			parentSessionName = resolved
		}
		reserved, capErr := reserveChildCapSlot(cfg, store, sessionName, parentSessionName, targetAlreadyUp)
		if capErr != nil {
			return nil, capErr
		}
		if reserved {
			defer releaseChildCapSlot(store, sessionName)
		}
		if existing == nil || hasIncompleteSessionTask(cfg, existing) {
			if _, err := Create(cfg, store, CreateParams{
				URL:           params.Identifier,
				Tag:           tag,
				Workflow:      params.Workflow,
				Inputs:        params.Inputs,
				ParentSession: params.ParentSession,
				Observer:      params.Observer,
				Population:    params.Population,
			}); err != nil {
				return nil, err
			}
		}
		identifier = sessionName
	} else {
		if params.Tag != "" {
			return nil, &Error{Code: ErrInvalidTag, Message: "--tag is only valid when the identifier is a resource a workflow resolver matches; a session name already encodes the tag"}
		}
		if params.Inputs != nil {
			return nil, &Error{Code: ErrInvalidInput, Message: "--input is only valid on the auto-create path; a bare session name implies an existing session"}
		}
	}

	sessionName, session, err := resolveSession(cfg, store, identifier)
	if err != nil {
		return nil, err
	}
	// This covers every path below, including callers that pass no
	// Observer at all.
	params.Observer = withNodeResultRecording(store, sessionName, params.Observer)
	// Bringing up an existing session runs run-scoped tasks against it; clamp
	// it to the active guard. The auto-create paths above already guard
	// via Create — this catches `plect up <bare-existing-session>`, which skips it.
	if guardErr := checkSessionGuard(cfg, sessionName); guardErr != nil {
		return nil, guardErr
	}
	// Mirrors the matched branch's reservation above, for a bare-name
	// session going straight from down to up.
	if !matched {
		targetAlreadyUp := sessionRunState(cfg, session) == domain.RunUp && !params.ForceRecreate
		reserved, capErr := reserveChildCapSlot(cfg, store, sessionName, session.ParentSession, targetAlreadyUp)
		if capErr != nil {
			return nil, capErr
		}
		if reserved {
			defer releaseChildCapSlot(store, sessionName)
		}
	}
	if params.ForceRecreate && !matched {
		forceRecreateExisting = true
	}
	if params.ForceRecreate && forceRecreateExisting {
		if guardErr := checkLifecycleRelationGuard(store, sessionName, "up --force-recreate"); guardErr != nil {
			return nil, guardErr
		}
	}
	if session.Nodes == nil {
		session.Nodes = make(map[string]*contract.TaskState)
	}
	if session.Tasks == nil {
		session.Tasks = make(map[string]*contract.TaskState)
	}

	plan, err := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	wf, wfErr := loadSessionWorkflow(cfg, session.WorkspaceDirPath, session)
	if wfErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: wfErr.Error()}
	}
	if params.ForceRecreate && forceRecreateExisting {
		var recreateErr error
		plan, recreateErr = recreateSessionRuntime(cfg, store, sessionName, session, wf, plan, params.Observer)
		if recreateErr != nil {
			return nil, recreateErr
		}
	} else if cleanupErr := cleanupStaleWorkflowNodes(cfg, store, sessionName, session, plan, params.Observer); cleanupErr != nil {
		return nil, cleanupErr
	}
	setupErr := runNodeSetup(context.Background(), plan.UpOrder(), sessionVars(cfg, session, plan), session, params.Observer)
	session.UpdatedAt = time.Now()
	// A run-scope node's setup script can itself shell out to a nested `plect
	// task setup` against this same session (e.g. goal_bootstrap re-deriving
	// pursue_goal instances, config/plect/tasks/goal_bootstrap.toml) while this
	// call's own RunSetup is still in flight. That nested call persists its
	// instance straight to disk under its own store.Update. A blind Put here
	// would then overwrite disk with our in-memory map, taken before the
	// nested write landed, silently dropping it — the same hazard mergeTasks
	// already exists to close for Create's initial_task dispatcher.
	if err := mergeTasks(store, sessionName, session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	if setupErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: setupErr.Error()}
	}
	// Reflect nested-written keys (merged onto disk above, not our in-memory
	// map) in the result the same way Create does. The merge above already
	// persisted successfully, so a failure here only means the returned
	// view may be stale, not that anything was lost.
	if refreshed, err := store.GetE(sessionName); err != nil {
		slog.Warn("read session state for post-merge refresh failed; returning the pre-refresh view", "session", sessionName, "error", err)
	} else if refreshed != nil {
		session = refreshed
	}
	if err := setSessionStatus(store, sessionName, contract.SessionStatusUp); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to record session status: %v", err)}
	}
	recordLifecycle(store, sessionName, "up", "run-scoped tasks produced")
	return &UpResult{SessionName: sessionName, Tasks: domain.MergedTasks(session)}, nil
}

func cleanupStaleWorkflowNodes(cfg *config.Config, store *state.Store, sessionName string, session *domain.Session, plan *task.Plan, observer task.Observer) error {
	stale, err := staleProducedWorkflowNodes(cfg, session, plan)
	if err != nil {
		return &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if len(stale) == 0 {
		return nil
	}
	cleanupErr := runTaskCleanup(context.Background(), stale, sessionVars(cfg, session, plan), session, observer)
	session.UpdatedAt = time.Now()
	if err := persistStaleWorkflowCleanup(store, sessionName, session, stale); err != nil {
		return &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	if refreshed, err := store.GetE(sessionName); err != nil {
		slog.Warn("read session state for post-cleanup refresh failed; returning the pre-refresh view", "session", sessionName, "error", err)
	} else if refreshed != nil {
		*session = *refreshed
		if session.Nodes == nil {
			session.Nodes = make(map[string]*contract.TaskState)
		}
		if session.Tasks == nil {
			session.Tasks = make(map[string]*contract.TaskState)
		}
	}
	if cleanupErr != nil {
		return &Error{Code: ErrExecutionFailed, Message: cleanupErr.Error()}
	}
	return nil
}

func persistStaleWorkflowCleanup(store *state.Store, sessionName string, session *domain.Session, stale []task.Resolved) error {
	return store.Update(sessionName, func(s *domain.Session) error {
		if s.Nodes == nil {
			s.Nodes = make(map[string]*contract.TaskState)
		}
		for _, r := range stale {
			st := session.Nodes[r.NodeID]
			if st == nil {
				continue
			}
			if st.Status == contract.TaskStatusCleaned {
				delete(s.Nodes, r.NodeID)
				continue
			}
			s.Nodes[r.NodeID] = st
		}
		s.UpdatedAt = session.UpdatedAt
		return nil
	})
}

func staleProducedWorkflowNodes(cfg *config.Config, session *domain.Session, plan *task.Plan) ([]task.Resolved, error) {
	if session == nil || len(session.Nodes) == 0 {
		return nil, nil
	}
	current := make(map[string]bool)
	for _, r := range plan.UpOrder() {
		current[r.NodeID] = true
	}
	defs, err := cfg.LoadTaskDefinitions(session.WorkspaceDirPath)
	if err != nil {
		return nil, fmt.Errorf("load task definitions: %w", err)
	}
	type seqResolved struct {
		seq int
		r   task.Resolved
	}
	var items []seqResolved
	for _, key := range sortedTaskKeys(session.Nodes) {
		st := session.Nodes[key]
		if st == nil || st.Status == contract.TaskStatusCleaned || key == contract.WorkflowPseudoNodeID || current[key] {
			continue
		}
		taskID := taskIDForInstance(key, st)
		def, ok := defs[taskID]
		if !ok {
			return nil, fmt.Errorf("stale workflow node %q uses unknown effect definition %q; cleanup cannot run", key, taskID)
		}
		items = append(items, seqResolved{
			seq: st.Seq,
			r: task.Resolved{
				NodeID:     key,
				TaskID:     taskID,
				Scope:      st.Scope,
				Cleanup:    def.Cleanup,
				SourcePath: def.SourcePath,
				From:       def.Ownership(),
				Layers:     effect.CleanupLayers(def),
			},
		})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].seq < items[j].seq })
	out := make([]task.Resolved, len(items))
	for i, it := range items {
		out[i] = it.r
	}
	return out, nil
}

func recreateSessionRuntime(cfg *config.Config, store *state.Store, sessionName string, session *domain.Session, wf config.WorkflowFile, teardownPlan *task.Plan, observer task.Observer) (*task.Plan, error) {
	if err := setSessionStatus(store, sessionName, contract.SessionStatusDown); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to record session status: %v", err)}
	}
	teardown, teardownErr := unifiedTeardownList(cfg, session, teardownPlan, false)
	if teardownErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: teardownErr.Error()}
	}
	cleanupErr := runTaskCleanup(context.Background(), teardown, sessionVars(cfg, session, teardownPlan), session, observer)
	session.UpdatedAt = time.Now()
	if cleanupErr != nil {
		if err := mergeTasks(store, sessionName, session); err != nil {
			return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
		}
		return nil, &Error{Code: ErrExecutionFailed, Message: cleanupErr.Error()}
	}

	if wfState, ok := session.Nodes[contract.WorkflowPseudoNodeID]; ok && wfState != nil {
		// Internal reset path, not an explicit operator destroy: no cleanup
		// intents to forward.
		if workflowCleanupErr := runWorkflowCleanupForDestroy(cfg, session, true, nil, observer); workflowCleanupErr != nil {
			session.UpdatedAt = time.Now()
			if err := mergeTasks(store, sessionName, session); err != nil {
				return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
			}
			return nil, &Error{Code: ErrExecutionFailed, Message: workflowCleanupErr.Error()}
		}
	}

	session.WorkspaceDirPath = ""
	session.Nodes = make(map[string]*contract.TaskState)
	session.Tasks = make(map[string]*contract.TaskState)
	session.Health = nil
	session.LastTickAt = time.Time{}
	session.TickBackoff = nil
	session.UpdatedAt = time.Now()
	if err := replaceRuntimeState(store, sessionName, session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}

	outputs, setupErr := runWorkflowSetupForSession(cfg, wf, session, observer)
	session.UpdatedAt = time.Now()
	if outputs != nil {
		if workspaceDir, ok := outputs[contract.OutputKeyWorkspaceDir].(string); ok {
			session.WorkspaceDirPath = workspaceDir
		}
		// A git-backed provider's own "branch" output (domain.SessionBranch)
		// needs no mirroring here: it already lives in this @workflow node's
		// Outputs, persisted by replaceRuntimeState below.
	}
	if err := replaceRuntimeState(store, sessionName, session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	if setupErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: setupErr.Error()}
	}

	setupPlan, planErr := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if planErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: planErr.Error()}
	}
	session.UpdatedAt = time.Now()
	if err := replaceRuntimeState(store, sessionName, session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	setupErr = runNodeSetup(context.Background(), setupPlan.Session, sessionVars(cfg, session, setupPlan), session, observer)
	session.UpdatedAt = time.Now()
	if err := mergeTasks(store, sessionName, session); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	if setupErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: setupErr.Error()}
	}
	return setupPlan, nil
}

func runWorkflowSetupForSession(cfg *config.Config, wf config.WorkflowFile, session *domain.Session, observer task.Observer) (map[string]any, error) {
	workspaceProviders, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		return nil, fmt.Errorf("load workspace providers: %w", err)
	}
	prov, ok, provErr := workspaceProviderFor(wf, workspaceProviders)
	if provErr != nil {
		return nil, provErr
	}
	if !ok {
		return nil, fmt.Errorf("workflow %q declares no workspace provider; its setup hook cannot run", wf.ID)
	}
	provInputs, inputsErr := resolveWorkspaceProviderInputs(prov, wf)
	if inputsErr != nil {
		return nil, inputsErr
	}
	vars := effect.WorkflowHookVars{
		ResourceID:        session.ResourceID,
		SessionName:       session.Name,
		WorkspaceDirsRoot: cfg.WorkspaceDirsRoot,
		SessionInputs:     session.Inputs,
		Inputs:            provInputs,
		Plugins:           cfg.Plugins,
		SourcePath:        prov.SourcePath,
	}
	return runWorkflowSetup(prov, vars, session, observer)
}
