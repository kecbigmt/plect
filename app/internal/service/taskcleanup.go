package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TaskCleanupParams are the inputs to TaskCleanup (the `plect task cleanup`
// path). Instance is the key to reclaim — a `--name` (e.g. `initial`) or a
// numbered `<task>#<n>`. SessionName defaults to the ambient pane env.
type TaskCleanupParams struct {
	Instance    string
	SessionName string
	Observer    task.Observer
}

// TaskCleanupResult reports the outcome. Found is false when the instance was
// absent (a no-op success — `cleanup` is idempotent, mirroring `docker rm` of a
// gone container being the safe default for a `cleanup; setup` dispatcher).
type TaskCleanupResult struct {
	SessionName string `json:"session_name"`
	Instance    string `json:"instance"`
	Found       bool   `json:"found"`
	// Unsubscribed is true only when reclaiming this instance also ran an
	// unsubscribe hook that dropped its bound resource's event-delivery
	// registration — not merely when nothing needed unregistering.
	Unsubscribed bool `json:"unsubscribed,omitempty"`
	// Resource is the instance's own bound resource, reported alongside
	// Unsubscribed/UnsubscribeError so a caller can say what was (or wasn't)
	// dropped.
	Resource string `json:"resource,omitempty"`
	// UnsubscribeError carries a failed unsubscribe attempt's message. Never
	// fails TaskCleanup itself: the instance and its own cleanup script
	// already succeeded by the time this runs. The failure is also queued
	// for a durable retry (see queuePendingUnsubscribe) since the reclaimed
	// instance record itself is gone and so is no longer a retry handle —
	// this field is what lets the caller learn about it now, immediately.
	UnsubscribeError string `json:"unsubscribe_error,omitempty"`
}

// TaskCleanup tears down a single dynamic instance: it runs that instance's
// cleanup script and removes it from session state. It is the single-instance
// counterpart of the down/destroy teardown — keyed by the instance handle alone,
// so it reclaims the named instance regardless of which task produced it (a
// task drift that left `initial` pointing at the wrong task is still swept).
//
// Absent instance is a no-op (Found=false, no error). Removing the entry rather
// than tombstoning it to `cleaned` frees the name, so the dispatcher's
// `cleanup <name>; setup … --name <name>` recreates without a collision.
func TaskCleanup(cfg *config.Config, store *state.Store, params TaskCleanupParams) (*TaskCleanupResult, error) {
	if strings.TrimSpace(params.Instance) == "" {
		return nil, &Error{Code: ErrInvalidInput, Message: "instance is required"}
	}

	sessionName := params.SessionName
	if sessionName == "" {
		sessionName = os.Getenv("PLECT_SESSION_NAME")
	}
	if sessionName == "" {
		return nil, &Error{Code: ErrInvalidInput, Message: "no session in scope: run inside a plect session pane (PLECT_SESSION_NAME) or pass --session"}
	}

	resolvedName, session, err := resolveSession(cfg, store, sessionName)
	if err != nil {
		return nil, err
	}
	params.Observer = withNodeResultRecording(store, resolvedName, params.Observer)
	flushPendingDeliveryLogged(cfg, store, resolvedName)

	st := session.Tasks[params.Instance]
	if st == nil {
		return &TaskCleanupResult{SessionName: resolvedName, Instance: params.Instance, Found: false}, nil
	}

	// Build the cleanup-relevant Resolved straight from the definition — no
	// schema / done_when validation (teardown stays resilient to a def whose
	// config drifted to invalid after the instance was created). Cleanup needs
	// only the script plus the persisted inputs/outputs/resource.
	taskID := instanceDefinitionAddress(params.Instance, st, nodeAddresses(cfg, session))
	r := task.Resolved{NodeID: params.Instance, TaskID: taskID, Scope: st.Scope}
	defs, defErr := cfg.LoadTaskDefinitions(session.WorkspaceDirPath)
	if defErr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("load task definitions: %v", defErr)}
	}
	if def, ok := defs[taskID]; ok {
		r.Cleanup = def.Cleanup
		r.SourcePath = def.SourcePath
		r.Layers = effect.CleanupLayers(def)
	}

	// RunCleanup mutates st (the snapshot's entry) in place. Persist only that one
	// key under the lock — a blind Put of the whole snapshot would clobber an
	// instance a concurrent TaskSetup reserved during this (unlocked) cleanup.
	// No full compiled Plan in scope here (this teardown targets a single
	// dynamic instance by its state entry, not a workflow node) — plect
	// attach/capture and a `{ terminal = "..." }` binding are unavailable in this
	// instance's own cleanup, the same as any sibling-task output it hasn't
	// explicitly depended on.
	cleanupErr := task.RunCleanup(context.Background(), []task.Resolved{r}, sessionVars(cfg, session, nil), session.Tasks, params.Observer)
	if cleanupErr != nil {
		// Keep the failed status inspectable for retry, scoped to this key. A
		// persist failure here means that inspectable status never lands, so it's
		// joined into the returned error rather than discarded.
		persistErr := store.Update(resolvedName, func(s *domain.Session) error {
			if cur := s.Tasks[params.Instance]; cur != nil {
				cur.Status = st.Status
				cur.Error = st.Error
				cur.FailedAt = st.FailedAt
				mergeLayerLifecycle(cur, st)
			}
			s.UpdatedAt = time.Now()
			return nil
		})
		return nil, &Error{Code: ErrExecutionFailed, Message: errors.Join(cleanupErr, persistErr).Error()}
	}

	if err := store.Update(resolvedName, func(s *domain.Session) error {
		delete(s.Tasks, params.Instance)
		s.UpdatedAt = time.Now()
		return nil
	}); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("failed to save session state: %v", err)}
	}
	recordLifecycle(store, resolvedName, "task_cleanup", fmt.Sprintf("reclaimed %s", params.Instance))

	result := &TaskCleanupResult{SessionName: resolvedName, Instance: params.Instance, Found: true, Resource: st.Resource}
	result.Unsubscribed, result.UnsubscribeError = unwireDeliveryOnTeardown(cfg, store, resolvedName, st.Resource)
	return result, nil
}

// mergeLayerLifecycle carries a partial unwind's per-layer outcome from the
// snapshot the cleanup mutated into the entry under the lock. Without it a
// retry reloads every layer as still produced and releases one that was
// already released — the exactly-the-owing-layer guarantee only holds if the
// record of what was owed survives the failure.
//
// Only the lifecycle fields move: a layer's outputs, locals, and environment
// belong to whatever wrote them last, which for a concurrent set-output on
// this same instance is the store's copy, not this stale snapshot.
func mergeLayerLifecycle(cur, snapshot *contract.TaskState) {
	if len(cur.Layers) != len(snapshot.Layers) {
		return
	}
	for i := range snapshot.Layers {
		if cur.Layers[i].EffectID != snapshot.Layers[i].EffectID {
			return
		}
	}
	for i := range snapshot.Layers {
		cur.Layers[i].Status = snapshot.Layers[i].Status
		cur.Layers[i].CleanedAt = snapshot.Layers[i].CleanedAt
		cur.Layers[i].FailedAt = snapshot.Layers[i].FailedAt
		cur.Layers[i].Error = snapshot.Layers[i].Error
	}
}
