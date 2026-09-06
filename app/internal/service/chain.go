package service

import (
	"sort"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/chain"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// Blocked-reason values for a chain that did not fire this evaluation.
const (
	chainBlockedWhenUnmet          = "when_unmet"
	chainBlockedOutputsMissing     = "outputs_missing"
	chainBlockedInvalidBindings    = "invalid_bindings"
	chainBlockedWorkflowUnresolved = "workflow_unresolved"
	chainBlockedResourceUnresolved = "resource_unresolved"
)

// ChainSpawn is one (chain, instance) evaluation: whether the chain fired and,
// if so, the spawned reviewer's placement and identity. CheckSession (plect
// status / plect_check) reports it as a dry-run plan (Spawned always false,
// so a fired-but-unspawned entry is reported as eligible, never as having
// actually fired); TickSession (plect tick) spawns each fired,
// not-already-active entry and fills Spawned/TargetSession/CapRefused in
// with the result.
type ChainSpawn struct {
	ChainID       string `json:"chain_id"`
	Task          string `json:"task,omitempty"`
	Instance      string `json:"instance"`
	Workflow      string `json:"workflow"`
	Placement     string `json:"placement"`
	Resource      string `json:"resource,omitempty"`
	Tag           string `json:"tag,omitempty"`
	ParentSession string `json:"parent_session,omitempty"`
	TargetSession string `json:"target_session,omitempty"`
	Fired         bool   `json:"fired"`
	BlockedReason string `json:"blocked_reason,omitempty"`
	// MissingOutputs names the input projections that had nothing to read
	// this evaluation — the facts the chain is waiting on before it fires.
	MissingOutputs []string       `json:"missing_outputs,omitempty"`
	Inputs         map[string]any `json:"inputs,omitempty"`
	Spawned        bool           `json:"spawned,omitempty"`
	AlreadyActive  bool           `json:"already_active,omitempty"`
	// CapRefused marks a fired, not-already-active chain whose spawn was
	// refused by the parent's max_up_children cap specifically, rather than
	// some other execution failure: the fire itself stays eligible (the
	// predicate is still true) and is retried, unblocked, on the next tick —
	// this only distinguishes the refusal reason for reporting.
	CapRefused    bool     `json:"cap_refused,omitempty"`
	KickDelivered bool     `json:"kick_delivered,omitempty"`
	KickDebounced bool     `json:"kick_debounced,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// pendingJudgeIDs lists, sorted, the judge leaf ids with no usable verdict at
// the current revision — the `task.done_when.pending_judge_ids` a chain input
// hands the spawned reviewer so it knows which leaves to judge.
func pendingJudgeIDs(facts chain.Facts) []string {
	var ids []string
	for id, jf := range facts.Judges {
		if jf.Pending {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// buildChainFacts projects the done_when evaluation into the raw-fact view a
// chain's `when` reads: the same live state, and judge leaves reduced to
// pending/current-action.
func buildChainFacts(state task.CompletionState, result task.DoneWhenResult) chain.Facts {
	judges := make(map[string]chain.JudgeFact)
	for _, leaf := range result.Leaves {
		if leaf.Kind != "judge" {
			continue
		}
		jf := chain.JudgeFact{Pending: leaf.Status == task.DonePending}
		switch leaf.Status {
		case task.DoneSatisfied:
			jf.Action = task.JudgeActionApprove
		case task.DoneUnsatisfied:
			jf.Action = task.JudgeActionRequestChanges
		}
		judges[leaf.ID] = jf
	}
	return chain.Facts{State: state, Judges: judges}
}

// placementParent maps a placement to the spawned session's parent: a child
// reviewer is parented under the work session; a sibling under the work
// session's own parent — or, when the work session has none, under its own
// implicit root (domain.ImplicitRootParent), so a parentless work session
// (e.g. an owner orchestrator pursuing a goal) can still spawn an independent
// sibling reviewer instead of sibling placement being unreachable there.
func placementParent(placement string, session *domain.Session, workName string) string {
	if placement == config.ChainPlacementChild {
		return workName
	}
	if session.ParentSession != "" {
		return session.ParentSession
	}
	return domain.ImplicitRootParent(workName)
}

// chainSpawnTag folds the chain id and instance into the spawned reviewer's
// session tag, keeping (chain, instance) pairs from colliding on one session.
func chainSpawnTag(chainID, instance string) string {
	return sanitizeTag(chainID + "-" + instance)
}

func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// resolveSpawnSessionName derives, offline, the session name a fire's spawn
// would resolve to — mirroring Up's name derivation (the workspace provider resolver) so
// the idempotency check sees the same name Up will. A resource no resolver
// matches is an identity dispatch, whose name is the resource itself.
func resolveSpawnSessionName(cfg *config.Config, resource, workflow, tag string) (string, error) {
	disp, matched, err := dispatchResource(cfg, workflow, resource)
	if err != nil {
		return "", err
	}
	if matched {
		name := disp.Name
		if tag != "" {
			name = name + "+" + tag
		}
		return name, nil
	}
	return resource, nil
}
