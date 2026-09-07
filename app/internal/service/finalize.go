package service

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// FinalizeTaskParams are the inputs to FinalizeTask (`plect task finalize`).
type FinalizeTaskParams struct {
	Instance    string
	SessionName string
}

// FinalizeTaskResult reports the finalization outcome.
type FinalizeTaskResult struct {
	SessionName string `json:"session_name"`
	Instance    string `json:"instance"`
	ResourceID  string `json:"resource_id,omitempty"`
	Definition  string `json:"resource_definition,omitempty"`
	Finalized   bool   `json:"finalized"`
}

// FinalizeTask is the generic finalization step ADR "goal-as-task" D4
// requires between a task instance's done_when being satisfied and its
// teardown: reconfirm the instance's done_when is satisfied at the current
// revision, then let the bound resource's definition record completion (if it
// declares a `finalize` script — most resource kinds don't, so this is a
// no-op for them). It is "gate + record"
// only — it never tears the instance down; the caller runs `plect task cleanup`
// separately once it's done observing the finalized instance. `cleanup`
// itself stays unaware of any of this (it never gains completion semantics —
// abort/destroy must still just tear down).
//
// Refuses (no record, instance untouched) when done_when is not currently
// satisfied — finalization must never be the thing that forces completion.
//
// Dynamic outputs are refreshed first, exactly as `tick` does before it
// evaluates: reconfirming against merely-persisted (possibly stale) outputs
// would let a revision approved before the last refresh — or before a new
// commit landed — pass a check it should no longer pass, and finalize an
// instance whose resource has since moved on.
func FinalizeTask(cfg *config.Config, store *state.Store, params FinalizeTaskParams) (*FinalizeTaskResult, error) {
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

	// finalize must reconfirm at the CURRENT revision, so an observation that
	// just failed leaves that reconfirmation unproven rather than merely
	// stale. Fail closed rather than record completion against whatever the
	// resource last said.
	observation, err := ObserveInstanceResource(cfg, store, sessionName, params.Instance)
	if err != nil {
		return nil, err
	}
	if observation != nil && observation.Error != "" {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("instance %q: observing its resource failed (%s); finalize refuses to reconfirm against a stale observation", params.Instance, observation.Error)}
	}
	resolvedName, session, err := resolveSession(cfg, store, sessionName)
	if err != nil {
		return nil, err
	}
	st := domain.TaskState(session, params.Instance)
	if st == nil {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("instance %q not found in session %s", params.Instance, resolvedName)}
	}

	declarations, err := loadDeclarations(cfg, session)
	if err != nil {
		return nil, err
	}
	dw, gateOutputs, derr := declarations.gate(params.Instance, st)
	if derr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: derr.Error()}
	}
	if dw == nil {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("instance %q declares no done_when; finalize has nothing to reconfirm", params.Instance)}
	}

	allSessions, err := store.AllE()
	if err != nil {
		return nil, err
	}
	eval := task.EvaluateTaskDoneWhenWithContext(dw, gateOutputs, doneWhenEvalContext(resolvedName, st, allSessions))
	if eval.Overall != task.DoneSatisfied {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("instance %q done_when is %s, not satisfied; finalize refuses to record completion or clean up", params.Instance, eval.Overall)}
	}

	resourceID := st.Resource
	if resourceID == "" {
		resourceID = session.ResourceID
	}
	result := &FinalizeTaskResult{SessionName: resolvedName, Instance: params.Instance, ResourceID: resourceID}

	if resourceID != "" {
		resDefs, rerr := cfg.LoadResourceDefs()
		if rerr != nil {
			return nil, &Error{Code: ErrExecutionFailed, Message: rerr.Error()}
		}
		ran, matched, ferr := task.FinalizeResource(resDefs, task.FinalizeResourceParams{
			ResourceID:  resourceID,
			SessionName: resolvedName,
			Revision:    instanceRevision(st),
			Judges:      judgeEvidenceFromLeaves(eval.Leaves),
			Plugins:     cfg.Plugins,
		})
		if ferr != nil {
			return nil, &Error{Code: ErrExecutionFailed, Message: ferr.Error()}
		}
		result.Finalized = ran
		result.Definition = matched.ID
	}

	if err := store.Update(resolvedName, func(s *domain.Session) error {
		if cur := domain.TaskState(s, params.Instance); cur != nil {
			cur.FinalizedAt = time.Now()
		}
		s.UpdatedAt = time.Now()
		return nil
	}); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("persist finalized_at: %v", err)}
	}

	return result, nil
}

// judgeEvidenceFromLeaves collects the satisfied judge leaves' evidence from a
// done_when evaluation, for the resource's finalize script to cite (who
// approved what, at which revision).
func judgeEvidenceFromLeaves(leaves []task.DoneLeafResult) []task.FinalizeJudgeEvidence {
	var out []task.FinalizeJudgeEvidence
	for _, leaf := range leaves {
		if leaf.Kind != "judge" {
			continue
		}
		out = append(out, task.FinalizeJudgeEvidence{
			ID:            leaf.ID,
			Reason:        leaf.Reason,
			Revision:      leaf.Revision,
			JudgeSession:  leaf.JudgeSession,
			JudgeWorkflow: leaf.JudgeWorkflow,
			Relation:      leaf.Relation,
		})
	}
	return out
}
