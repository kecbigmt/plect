package service

import (
	"os"
	"slices"
	"sort"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// PopulationTaskBlockers evaluates every dynamic task through the ordinary
// completion path. A missing policy or any result other than satisfied is a
// blocker because automatic population teardown must fail closed.
func PopulationTaskBlockers(cfg *config.Config, store *state.Store, sessionName string) ([]string, error) {
	if _, err := ObserveSessionResources(cfg, store, sessionName); err != nil {
		return nil, err
	}
	resolvedName, session, err := resolveSession(cfg, store, sessionName)
	if err != nil {
		return nil, err
	}
	declarations, err := loadDeclarations(cfg, session)
	if err != nil {
		return nil, err
	}
	allSessions, err := store.AllE()
	if err != nil {
		return nil, err
	}
	var blockers []string
	for _, key := range sortedTaskKeys(session.Tasks) {
		st := session.Tasks[key]
		if st == nil || !st.Dynamic || st.Status == contract.TaskStatusCleaned {
			continue
		}
		if st.Status != contract.TaskStatusProduced {
			blockers = append(blockers, key)
			continue
		}
		dw, live, gateErr := declarations.gate(key, st)
		if gateErr != nil {
			return nil, gateErr
		}
		if dw == nil {
			blockers = append(blockers, key)
			continue
		}
		result := task.EvaluateTaskDoneWhenWithContext(dw, live, doneWhenEvalContext(resolvedName, st, allSessions))
		if result.Overall != task.DoneSatisfied {
			blockers = append(blockers, key)
		}
	}
	sort.Strings(blockers)
	return blockers, nil
}

// CheckParams has no refresh option: check always reads persisted state (see
// CheckSession). Only tick refreshes dynamic outputs before evaluating.
type CheckParams struct {
	SessionName string
}

type CheckAction struct {
	SessionName    string `json:"session_name"`
	Instance       string `json:"instance"`
	Action         string `json:"action"`
	HeartbeatTicks int    `json:"heartbeat_ticks,omitempty"`
	// Layer names the layer of a nesting chain a budget escalation is
	// attributed to, empty for an instance-level one. LayerTicks carries
	// every layer's next tick count, since a chain advances each layer's
	// patience independently.
	Layer               string           `json:"layer,omitempty"`
	LayerIndex          int              `json:"-"`
	LayerTicks          []int            `json:"layer_ticks,omitempty"`
	HeartbeatBudget     int              `json:"heartbeat_budget,omitempty"`
	HeartbeatEscalation int              `json:"heartbeat_escalation,omitempty"`
	Items               []string         `json:"items,omitempty"`
	UnmetItems          []CheckUnmetItem `json:"unmet_items,omitempty"`
	Warnings            []string         `json:"warnings,omitempty"`
	Summary             string           `json:"summary,omitempty"`
	Body                string           `json:"body,omitempty"`
	ReviewerCommand     string           `json:"reviewer_command,omitempty"`
	JudgeCommands       []string         `json:"judge_commands,omitempty"`
	Fingerprint         string           `json:"fingerprint,omitempty"`
	EscalationKind      string           `json:"escalation_kind,omitempty"`
	HeartbeatChanged    bool             `json:"-"`
}

type CheckResult struct {
	Actions []CheckAction `json:"actions,omitempty"`
	// Chains is the [[chains]] evaluation for this same tick/check — fired /
	// already-active / blocked, with the reason — evaluated against the same
	// facts as Actions. CheckSession (and plect status, which shares the same
	// evaluation) always reports it as a dry-run plan (Spawned is always
	// false, so a fired entry is reported by its callers as eligible, not as
	// having fired); TickSession spawns each fired, not-already-active entry
	// and, on a cap refusal specifically, marks CapRefused instead of a
	// generic spawn failure.
	Chains []ChainSpawn `json:"chains,omitempty"`
}

type CheckUnmetItem struct {
	Kind             string          `json:"kind"`
	Expr             string          `json:"expr"`
	Status           task.DoneStatus `json:"status"`
	ID               string          `json:"id,omitempty"`
	Output           string          `json:"output,omitempty"`
	Value            string          `json:"value,omitempty"`
	Observed         bool            `json:"observed,omitempty"`
	Action           string          `json:"action,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	PendingReason    string          `json:"pending_reason,omitempty"`
	Revision         string          `json:"revision,omitempty"`
	CurrentRevision  string          `json:"current_revision,omitempty"`
	ReviewerSession  string          `json:"reviewer_session,omitempty"`
	ReviewerWorkflow string          `json:"reviewer_workflow,omitempty"`
	Relation         string          `json:"relation,omitempty"`
}

// computedAction bundles one instance's done_when evaluation with the
// record-time facts needed only by the actuator (plect tick): which instance
// key it belongs to, and the action/fingerprint the previous tick persisted
// for it (what an unchanged re-evaluation is recognized against).
// CheckSession discards these extras and reports only the action; TickSession
// consumes all of them. result is the raw leaf-level evaluation the action was
// derived from — Status's work layer reuses it instead of re-evaluating
// done_when for the same instance.
type computedAction struct {
	instance        string
	action          CheckAction
	lastAction      string
	lastFingerprint string
	result          task.DoneWhenResult
}

// evaluateSessionActions runs the read-only half shared by CheckSession (plect
// status / plect_check) and TickSession (plect tick): optionally observe each
// instance's resource, resolve the session, and evaluate done_when — and,
// against those same facts, [[chains]] — for every live task-document
// instance. It never writes state, spawns a session, or publishes events:
// CheckSession returns its result verbatim (a dry-run chain plan), while
// TickSession additionally publishes/persists per action and spawns each
// fired, not-already-active chain. observe is false for every CheckSession
// call: check reads persisted state only, so that repeated calls cannot
// themselves change what a session reports.
func evaluateSessionActions(cfg *config.Config, store *state.Store, sessionName string, observe bool, trigger TickTrigger) (string, []computedAction, []ChainSpawn, error) {
	if sessionName == "" {
		sessionName = os.Getenv("PLECT_SESSION_NAME")
	}
	if sessionName == "" {
		return "", nil, nil, &Error{Code: ErrInvalidInput, Message: "no session in scope: pass a session or run inside a plect session pane"}
	}
	if observe {
		// Observation comes first and lands in state before anything reads
		// it, so every leaf of this pass — a completion predicate and the
		// chain conditions beside it — decides against one snapshot.
		if _, err := ObserveSessionResources(cfg, store, sessionName); err != nil {
			return "", nil, nil, err
		}
	}
	resolvedName, session, err := resolveSession(cfg, store, sessionName)
	if err != nil {
		return "", nil, nil, err
	}
	allSessions, err := store.AllE()
	if err != nil {
		return "", nil, nil, err
	}
	// An id that resolves to no task document declares no completion
	// predicate: an effect brings something up and takes it down and answers
	// for nothing beyond that. Loading both kinds is still what resolves the
	// id, so a collision is reported rather than silently choosing a side.
	docs, _, err := loadTaskDeclarations(cfg, session)
	if err != nil {
		return "", nil, nil, err
	}
	var computed []computedAction
	var chainPlan []ChainSpawn
	for _, key := range sortedTaskKeys(session.Tasks) {
		st := session.Tasks[key]
		if st == nil || st.Status != contract.TaskStatusProduced || key == contract.WorkflowPseudoNodeID {
			continue
		}
		// A declaration-less instance is evaluated only when it was set up
		// with leaves of its own: an effect answers for no completion, but
		// `--done-when-json` conditions belong to the instance, not to a
		// declaration.
		doc := docs[taskIDForInstance(key, st)]
		if doc.DoneWhen == nil && len(doc.Chains) == 0 && len(st.ExtraDoneWhen) == 0 {
			continue
		}
		action, spawns, derr := evaluateDocumentInstance(cfg, store, doc, resolvedName, session, key, st, allSessions, trigger)
		if derr != nil {
			return "", nil, nil, derr
		}
		if action != nil {
			computed = append(computed, *action)
		}
		chainPlan = append(chainPlan, spawns...)
	}
	return resolvedName, computed, chainPlan, nil
}

// CheckSession reports the same done_when/chain evaluation tick would act on,
// but only reports it: no heartbeat budget advances, no event is published,
// no session is woken or spawned, and no dynamic output is refreshed. It reads
// whatever tick or initial produce last persisted. Calling it any number of
// times leaves state, event log, and session list unchanged. Use plect tick to
// actually advance the gate, refresh outputs, and fire chains.
func CheckSession(cfg *config.Config, store *state.Store, params CheckParams) (*CheckResult, error) {
	_, computed, chainPlan, err := evaluateSessionActions(cfg, store, params.SessionName, false, "")
	if err != nil {
		return nil, err
	}
	actions := make([]CheckAction, 0, len(computed))
	for _, c := range computed {
		actions = append(actions, c.action)
	}
	return &CheckResult{Actions: actions, Chains: chainPlan}, nil
}

func sessionResourceForCheck(session *domain.Session, st *contract.TaskState) string {
	if st.Resource != "" {
		return st.Resource
	}
	return session.ResourceID
}

func sortedTaskKeys(tasks map[string]*contract.TaskState) []string {
	keys := make([]string, 0, len(tasks))
	for key := range tasks {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
