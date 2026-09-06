package service

import (
	"fmt"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/chain"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
)

// evalDocumentChain decides one (chain, document instance) pair: whether it
// fires, where a fire would put the spawned session, and what its
// `[chains.inputs]` projections resolve to.
//
// The reference checks a chain carries — the target workflow, the judge ids
// its trigger names, the keys its projections read — are resolved at load
// against the contracts that declare them, so what is left here is the part
// that can only be known now: whether the trigger holds against this
// instance's live facts, and whether every projection has something to read.
func evalDocumentChain(cfg *config.Config, store *state.Store, def config.DocumentChain, workName string, session *domain.Session, instance, resource string, facts chain.Facts) ChainSpawn {
	placement := def.EffectivePlacement()
	sp := ChainSpawn{
		ChainID:       def.ID,
		Instance:      instance,
		Workflow:      def.Workflow,
		Placement:     placement,
		Resource:      resource,
		Tag:           chainSpawnTag(def.ID, instance),
		ParentSession: placementParent(placement, session, workName),
	}
	// The trigger is decided before anything else is resolved, so an
	// unresolvable projection is reported only for a chain that would
	// otherwise really fire — the same discipline evalChain applies to the
	// workflow-existence check.
	if !chain.WhenSatisfied(def.When, facts) {
		sp.BlockedReason = chainBlockedWhenUnmet
		return sp
	}
	// A named resource decides what the spawn binds to, so it is resolved
	// before the tag and the target session are derived from it. Nothing to
	// read means the fire waits: a session bound to nothing would resolve to
	// no provider and could never be the session the chain meant to spawn.
	if def.Resource != nil {
		named, err := resolveChainResource(def, workName, session.Workflow, instance, facts)
		if err != nil {
			sp.BlockedReason = chainBlockedResourceUnresolved
			sp.Warnings = append(sp.Warnings, fmt.Sprintf("the resource this chain spawns against could not be resolved: %v", err))
			return sp
		}
		if named == "" {
			sp.BlockedReason = chainBlockedResourceUnresolved
			return sp
		}
		sp.Resource = named
		resource = named
	}
	inputs, missing, err := resolveDocumentChainInputs(def, workName, session.Workflow, instance, facts)
	switch {
	case err != nil:
		sp.BlockedReason = chainBlockedInvalidBindings
		sp.Warnings = append(sp.Warnings, fmt.Sprintf("input projections could not be resolved: %v", err))
		return sp
	case len(missing) > 0:
		sp.BlockedReason = chainBlockedOutputsMissing
		sp.MissingOutputs = missing
		return sp
	}
	if len(inputs) > 0 {
		resolved, vErr := resolveSessionInputs(cfg, session.WorkspaceDirPath, def.Workflow, inputs)
		if vErr != nil {
			sp.BlockedReason = chainBlockedInvalidBindings
			sp.Warnings = append(sp.Warnings, fmt.Sprintf("resolved inputs violate workflow %q inputs contract: %s", def.Workflow, vErr.Message))
			return sp
		}
		sp.Inputs = resolved
	}
	sp.Fired = true
	if name, err := resolveSpawnSessionName(cfg, resource, def.Workflow, sp.Tag); err == nil && name != "" {
		sp.TargetSession = name
		existing, getErr := store.GetE(name)
		if getErr != nil {
			// Fail closed: an unreadable store cannot prove the target is
			// absent, and treating it as absent risks spawning a duplicate
			// session for a chain that is already active.
			sp.AlreadyActive = true
			sp.Warnings = append(sp.Warnings, fmt.Sprintf("could not check whether %q is already active: %v", name, getErr))
		} else if existing != nil && existing.Population == nil {
			sp.AlreadyActive = true
		}
	}
	return sp
}

// resolveChainResource evaluates the resource a fire binds its spawned
// session to, in two steps because the two failures mean opposite things. A
// projection that found nothing is the waiting case and resolves to the empty
// string: the pull request does not exist yet. A value that was found but
// cannot be carried as a resource identifier — a list, a table — is a fault,
// and is returned as one so the fire is blocked with the reason said out
// loud rather than waiting forever on a fact that has already arrived.
func resolveChainResource(def config.DocumentChain, workName, workflow, instance string, facts chain.Facts) (string, error) {
	eval := lang.Eval{Roots: documentChainRoots(workName, workflow, instance, facts)}
	resolved, absent, err := eval.Value(def.Resource)
	switch {
	// A projection's only evaluation failure is a path that resolved to
	// nothing; anything else this value can be is a computation, whose
	// failure is the author's.
	case err != nil && def.Resource.Form == lang.FormFrom:
		return "", nil
	case err != nil:
		return "", err
	case absent:
		return "", nil
	}
	text, err := lang.Stringify(resolved)
	if err != nil {
		return "", fmt.Errorf("%w; a resource is one identifier", err)
	}
	return strings.TrimSpace(text), nil
}

// resolveDocumentChainInputs evaluates each `[chains.inputs]` projection
// against the facts of the instance that fired and the two live roots it
// reads. A projection reaching a declared key nothing has reported yet is
// returned as missing rather than as an error: that is the firing gate — a
// chain waits for the fact instead of spawning a session wired to nothing.
func resolveDocumentChainInputs(def config.DocumentChain, workName, workflow, instance string, facts chain.Facts) (map[string]any, []string, error) {
	eval := lang.Eval{Roots: documentChainRoots(workName, workflow, instance, facts)}
	out := make(map[string]any, len(def.Inputs))
	var missing []string
	for _, key := range def.InputKeys() {
		value := def.Inputs[key]
		resolved, absent, err := eval.Value(value)
		switch {
		case err != nil && value.Form == lang.FormFrom:
			missing = append(missing, value.From)
		case err != nil:
			return nil, nil, fmt.Errorf("input %q: %w", key, err)
		case absent:
		default:
			out[key] = resolved
		}
	}
	return out, missing, nil
}

// documentChainRoots is what a chain's inputs are evaluated against: the
// facts of the instance that fired, and the same two live roots its trigger
// read.
func documentChainRoots(workName, workflow, instance string, facts chain.Facts) lang.Roots {
	roots := facts.State.Roots()
	roots["task"] = map[string]any{
		"session":  workName,
		"instance": instance,
		"workflow": workflow,
		// The pending ids reach the spawned reviewer as one scalar, so a
		// `for id in ...` setup can iterate them; a list has nowhere to go in
		// a session input declared as a string.
		"done_when": map[string]any{"pending_judge_ids": strings.Join(pendingJudgeIDs(facts), " ")},
	}
	return roots
}
