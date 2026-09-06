package service

import (
	"fmt"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// TickParams carries SkipRefresh, unlike CheckParams: check never refreshes,
// so only tick needs a way to suppress its (default-on) refresh.
type TickParams struct {
	SessionName string
	SkipRefresh bool
	Observer    task.Observer
	Trigger     TickTrigger
}

// TickTrigger records why the actuator ran. Only heartbeat-triggered ticks
// consume the done_when heartbeat budget.
type TickTrigger string

const (
	TickTriggerManual    TickTrigger = "manual"
	TickTriggerEvent     TickTrigger = "event"
	TickTriggerHeartbeat TickTrigger = "heartbeat"
)

// TickSession is the Goal Loop actuator: it refreshes outputs unless
// SkipRefresh, evaluates done_when for each produced task instance, and
// carries out the result. Heartbeat-triggered ticks consume the done_when
// heartbeat budget; event and manual ticks do not. Satisfied and escalated
// actions push terminal events to the parent, while review_required and kick
// publish same-session events that drive the reviewer or work session. Against
// that same refreshed fact set, it also fires [[chains]].
func TickSession(cfg *config.Config, store *state.Store, params TickParams) (*CheckResult, error) {
	resolvedName, computed, chainPlan, err := evaluateSessionActions(cfg, store, params.SessionName, !params.SkipRefresh, params.Trigger)
	if err != nil {
		return nil, err
	}

	// Stamp unconditionally (even when no instance has a computed action): a
	// tick always resets the `heartbeat` clock the reactor tracks,
	// whether or not anything was found unsatisfied in this tick.
	if err := stampLastTick(store, resolvedName); err != nil {
		return nil, err
	}

	var actions []CheckAction
	for _, c := range computed {
		action := c.action
		// Publish before persisting the marker: a publish failure must leave
		// LastAction/fingerprint unadvanced so the next tick retries this same
		// action instead of silently skipping delivery.
		warnings, err := publishTickAction(cfg, store, resolvedName, c, params.Trigger)
		if err != nil {
			return nil, err
		}
		action.Warnings = append(action.Warnings, warnings...)
		actions = append(actions, action)
		if err := persistTickAction(store, resolvedName, c.instance, action); err != nil {
			return nil, err
		}
	}

	chains := make([]ChainSpawn, 0, len(chainPlan))
	for _, sp := range chainPlan {
		if sp.Fired && !sp.AlreadyActive {
			up, err := Up(cfg, store, UpParams{
				Identifier:    sp.Resource,
				Tag:           sp.Tag,
				Workflow:      sp.Workflow,
				Inputs:        sp.Inputs,
				ParentSession: sp.ParentSession,
				Observer:      params.Observer,
			})
			// A spawn failure (e.g. a transient session/runtime error) must not
			// discard the done_when actions already published/persisted above,
			// nor the other chains' results — it is reported on this entry only,
			// so the next tick can retry the same (idempotent) fire.
			switch svcErr, isCapRefusal := asChildCapExceeded(err); {
			case isCapRefusal:
				// A cap refusal is not a generic execution failure: the fire
				// remains eligible and this same entry is retried next tick
				// once capacity frees, so it is reported as its own typed
				// outcome rather than folded into "spawn failed:".
				sp.CapRefused = true
				sp.Warnings = append(sp.Warnings, svcErr.Message)
				if pubErr := publishChainCapAttempt(cfg, store, resolvedName, sp); pubErr != nil {
					sp.Warnings = append(sp.Warnings, fmt.Sprintf("chain-attempt event failed: %v", pubErr))
				}
			case err != nil:
				sp.Warnings = append(sp.Warnings, fmt.Sprintf("spawn failed: %v", err))
			default:
				sp.Spawned = true
				sp.TargetSession = up.SessionName
			}
		} else if sp.Fired && sp.AlreadyActive {
			delivered, err := publishAlreadyActiveChainKick(cfg, store, resolvedName, sp)
			if err != nil {
				sp.Warnings = append(sp.Warnings, fmt.Sprintf("already-active kick failed: %v", err))
			} else if delivered {
				sp.KickDelivered = true
			} else {
				sp.KickDebounced = true
			}
		}
		chains = append(chains, sp)
	}

	return &CheckResult{Actions: actions, Chains: chains}, nil
}
