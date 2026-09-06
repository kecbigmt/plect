package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// DestroyParams holds parameters for Destroy.
type DestroyParams struct {
	Identifier string
	Force      bool
	// CleanupInputs are opaque key/value intents forwarded verbatim to the
	// workspace provider cleanup hook (see
	// effect.WorkflowHookVars.CleanupInputs); core interprets none of them.
	CleanupInputs map[string]string
	Observer      task.Observer
}

// DestroyResult holds the outcome of Destroy.
type DestroyResult struct {
	SessionName         string `json:"session_name"`
	RemovedWorkspaceDir bool   `json:"removed_workspace_dir"`
	// CleanupWarnings carries task cleanup errors that were downgraded to
	// warnings by --force. Without --force a cleanup error aborts Destroy and
	// returns the error directly; this field is only populated when the user
	// explicitly opted into best-effort teardown.
	CleanupWarnings []string `json:"cleanup_warnings,omitempty"`
}

// Destroy is the task-aware teardown path. fail-fast by default so a
// cleanup error leaves the partial state inspectable for retry; --force
// demotes cleanup errors to warnings so a stuck session can be freed
// without manual cleanup. State is persisted before each subsequent step
// so a mid-teardown crash stays inspectable. State-delete failures error
// even under --force — silent partial teardown would be worse than a
// noisy one.
func Destroy(cfg *config.Config, store *state.Store, params DestroyParams) (*DestroyResult, error) {
	sessionName, session, err := resolveSession(cfg, store, params.Identifier)
	if err != nil {
		return nil, err
	}
	flushPendingDeliveryLogged(cfg, store, sessionName)
	// Tearing down an existing session is a per-session write; clamp it to the
	// active guard so a guarded orchestrator can't destroy another owner's
	// session it can see via `plect ls`. Create guards on the way in; this
	// closes the symmetric teardown vector.
	if guardErr := checkSessionGuard(cfg, sessionName); guardErr != nil {
		return nil, guardErr
	}
	if guardErr := checkLifecycleRelationGuard(store, sessionName, "destroy"); guardErr != nil {
		return nil, guardErr
	}
	if session.Tasks == nil {
		session.Tasks = make(map[string]*contract.TaskState)
	}

	result := &DestroyResult{SessionName: sessionName}

	// Fail-closed before any teardown side effect: store.Delete unconditionally
	// clears ParentSession on every child, and plect up never re-adopts an
	// orphan, so a silent destroy permanently severs the tree. --force makes
	// that orphaning an explicit, reported choice instead.
	allSessions, err := store.AllE()
	if err != nil {
		// An unreadable store must never read as "no children": store.Delete
		// unconditionally clears ParentSession on every child, so proceeding
		// on a fabricated empty child list would silently orphan a real one.
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("read session state: %v", err)}
	}
	if children := childNames(allSessions, sessionName); len(children) > 0 {
		if !params.Force {
			return nil, &Error{
				Code: ErrHasChildren,
				Message: fmt.Sprintf(
					"session %s has %d child session(s) that would be orphaned: %s\nUse `plect down %s` + `plect up %s` to reset without orphaning them, or re-run with `plect destroy %s --force` to destroy and orphan them.",
					sessionName, len(children), strings.Join(children, ", "), sessionName, sessionName, sessionName,
				),
			}
		}
		result.CleanupWarnings = append(result.CleanupWarnings, fmt.Sprintf("orphaned %d child session(s): %s", len(children), strings.Join(children, ", ")))
	}

	plan, err := buildPlanForSession(cfg, session.WorkspaceDirPath, session)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}

	// A single reverse-instantiation teardown over every non-@workflow task —
	// static plan nodes (run + session) and dynamic instances merged into one
	// seq-descending pass. This is strictly the reverse of the instantiation
	// stack, so a static node instantiated after a dynamic one is still
	// cleaned first regardless of scope. @workflow (workspace) is released
	// last, below.
	teardown, teardownErr := unifiedTeardownList(cfg, session, plan, false)
	if teardownErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: teardownErr.Error()}
	}
	if cleanupErr := task.RunCleanup(context.Background(), teardown, sessionVars(cfg, session, plan), session.Tasks, params.Observer); cleanupErr != nil {
		session.UpdatedAt = time.Now()
		putBestEffort(store, session, "run cleanup failure")
		if !params.Force {
			return nil, &Error{Code: ErrExecutionFailed, Message: cleanupErr.Error()}
		}
		result.CleanupWarnings = append(result.CleanupWarnings, fmt.Sprintf("cleanup: %v", cleanupErr))
	}

	// Persist any TaskState changes (status flips to cleaned) before we
	// delete the entry, in case workspace directory removal fails and the
	// user wants to inspect the persisted checkpoint post hoc.
	session.UpdatedAt = time.Now()
	putBestEffort(store, session, "post-run-cleanup checkpoint")

	if wfState, ok := session.Tasks[contract.WorkflowPseudoNodeID]; ok && wfState != nil {
		// Workflow setup acquired the workspace, so workflow cleanup owns
		// its release — the core performs no workspace directory removal
		// here. (Whether the workspace directory is actually deleted is the
		// cleanup script's decision; setup/cleanup symmetry is the author's
		// contract.)
		cleanupErr := runWorkflowCleanupForDestroy(cfg, session, params.Force, params.CleanupInputs, params.Observer)
		session.UpdatedAt = time.Now()
		putBestEffort(store, session, "workflow cleanup for destroy")
		if cleanupErr != nil {
			if !params.Force {
				return nil, &Error{
					Code:    ErrExecutionFailed,
					Message: fmt.Sprintf("%v (session %s)\nRe-run with `plect destroy %s --force` to delete the state entry anyway.", cleanupErr, sessionName, sessionName),
				}
			}
			result.CleanupWarnings = append(result.CleanupWarnings, fmt.Sprintf("workflow cleanup: %v", cleanupErr))
		}
		result.RemovedWorkspaceDir = session.WorkspaceDirPath != "" && !fileExists(session.WorkspaceDirPath)
	}

	// Snapshot the state entry as a tombstone in the event log directory
	// before it's deleted, so resource mapping / judge records / final
	// outputs survive destroy. Fail-closed and unconditional on --force:
	// a lost tombstone is exactly the silent context loss this exists to prevent.
	destroyedAt := time.Now()
	tombstone := contract.Tombstone{Session: *session, DestroyedAt: destroyedAt}
	tombstoneData, merr := json.Marshal(tombstone)
	if merr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to marshal tombstone: %v", merr)}
	}
	if werr := eventlog.NewStore(store.Dir()).WriteTombstone(sessionName, tombstoneData); werr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to write tombstone: %v", werr)}
	}

	// Record after the tombstone succeeds; otherwise a failed destroy would
	// leave a lifecycle event claiming the session was destroyed.
	recordLifecycle(store, sessionName, "destroyed", "session destroyed")

	if err := store.Delete(sessionName); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to delete state entry: %v", err)}
	}

	// After the delete, so unwireDeliveryOnTeardown's fresh read sees the
	// session as gone rather than skipping the unsubscribe as still needed.
	if _, errMsg := unwireDeliveryOnTeardown(cfg, store, sessionName, session.ResourceID); errMsg != "" {
		result.CleanupWarnings = append(result.CleanupWarnings, fmt.Sprintf("resource delivery unsubscribe: %s", errMsg))
	}

	return result, nil
}
