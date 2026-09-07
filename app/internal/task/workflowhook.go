package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// workflowHookScope is the Observer scope label for pseudo-node events.
const workflowHookScope = "workflow"

// RunWorkflowSetup executes the workspace provider setup hook (the
// workflow-level lifecycle) and persists the result as the @workflow
// pseudo-node in tasks. Semantics mirror RunSetup: idempotent (an
// already-produced pseudo-node is skipped after its liveness check passes,
// repaired via force cleanup then setup when it fails), `prev.*` carries
// the prior outputs across retries, stdout is the JSON outputs contract.
//
// Additional contract: the outputs MUST contain the reserved `workspace_dir`
// key (non-empty string) — every downstream consumer (cascade resolution,
// task cwd, cd/attach) depends on it. The outputs are validated against the
// workflow's outputs schema when one is declared.
//
// Returns the pseudo-node outputs (whether fresh or reused).
func RunWorkflowSetup(prov config.WorkspaceProviderConfig, vars effect.WorkflowHookVars, tasks map[string]*contract.TaskState, observer Observer) (outputs map[string]any, repaired bool, err error) {
	obs := observerOr(observer)
	id := contract.WorkflowPseudoNodeID

	if existing, ok := tasks[id]; ok && existing != nil && existing.Status == contract.TaskStatusProduced {
		if aliveErr := checkWorkflowLiveness(prov, vars, existing.Outputs); aliveErr == nil {
			obs.OnSkip(workflowHookScope, id, "already produced")
			return existing.Outputs, false, nil
		} else if cleanupErr := repairWorkflowProvider(prov, vars, tasks, obs, aliveErr); cleanupErr != nil {
			return nil, false, cleanupErr
		}
		repaired = true
	}
	if prov.Setup == nil {
		return nil, repaired, fmt.Errorf("workspace provider %q declares no setup", prov.ID)
	}

	obs.OnStart(workflowHookScope, id)
	now := time.Now()
	var prev map[string]any
	if existing, ok := tasks[id]; ok && existing != nil {
		prev = existing.Outputs
	}

	fail := func(errMsg string) {
		tasks[id] = &contract.TaskState{
			Scope:    contract.TaskScopeSession,
			Status:   contract.TaskStatusFailed,
			Outputs:  prev,
			FailedAt: now,
			Error:    errMsg,
		}
	}

	eval := effect.ProviderEval(effect.ProviderRoots(vars, prev, nil, false), vars.Plugins, vars.SourcePath, prov.Ownership())
	stdout, stderr, runErr := effect.RunProviderAction(prov.Setup, eval)
	if runErr != nil {
		fail(runErr.Error())
		wrapped := fmt.Errorf("workspace provider %q setup: %w", prov.ID, runErr)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return nil, repaired, wrapped
	}
	outputs, parseErr := lang.ParseOutputs(stdout)
	if parseErr != nil {
		fail(parseErr.Error())
		wrapped := fmt.Errorf("workspace provider %q setup: %w", prov.ID, parseErr)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return nil, repaired, wrapped
	}
	workspaceDir, _ := outputs[contract.OutputKeyWorkspaceDir].(string)
	if strings.TrimSpace(workspaceDir) == "" {
		msg := fmt.Sprintf("setup outputs must contain a non-empty %q string (got %v)", contract.OutputKeyWorkspaceDir, outputs[contract.OutputKeyWorkspaceDir])
		fail(msg)
		wrapped := fmt.Errorf("workspace provider %q setup: %s", prov.ID, msg)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return nil, repaired, wrapped
	}
	schema, err := lang.CompileSchema(prov.OutputsSchema, prov.ResolvedOutputsSchemaPath(), "plect:workspace_provider:"+prov.ID+":outputs")
	if err != nil {
		fail(err.Error())
		wrapped := fmt.Errorf("workspace provider %q outputs schema: %w", prov.ID, err)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return nil, repaired, wrapped
	}
	if schema != nil {
		if vErr := schema.Validate(outputs); vErr != nil {
			fail(vErr.Error())
			wrapped := fmt.Errorf("workspace provider %q setup: outputs schema: %w", prov.ID, vErr)
			obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
			return nil, repaired, wrapped
		}
	}

	tasks[id] = &contract.TaskState{
		Scope:   contract.TaskScopeSession,
		Status:  contract.TaskStatusProduced,
		Outputs: outputs,
		Seq:     nextSeq(tasks),
		SetupAt: now,
	}
	obs.OnSuccess(workflowHookScope, id, time.Since(now), stderr)
	return outputs, repaired, nil
}

// checkWorkflowLiveness runs a provider's [health].alive probe; a nil probe
// passes without running anything.
func checkWorkflowLiveness(prov config.WorkspaceProviderConfig, vars effect.WorkflowHookVars, outputs map[string]any) error {
	action := prov.Health.AliveProbe()
	if action == nil || action.Type == lang.ActionNoop {
		return nil
	}
	eval := effect.ProviderEval(effect.AliveRoots(vars, outputs), vars.Plugins, vars.SourcePath, prov.Ownership())
	_, stderr, err := effect.RunProviderAction(action, eval)
	if err != nil {
		if len(stderr) > 0 {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(stderr)))
		}
		return err
	}
	return nil
}

// repairWorkflowProvider stamps the liveness failure onto the pseudo-node,
// mirroring invalidateProducedNode's convention, then force-cleans it.
func repairWorkflowProvider(prov config.WorkspaceProviderConfig, vars effect.WorkflowHookVars, tasks map[string]*contract.TaskState, obs Observer, aliveErr error) error {
	id := contract.WorkflowPseudoNodeID
	existing := tasks[id]
	existing.Status = contract.TaskStatusFailed
	existing.Error = aliveErr.Error()
	existing.FailedAt = time.Now()
	forced := vars
	forced.Force = true
	if err := RunWorkflowCleanup(prov, forced, tasks, obs); err != nil {
		return fmt.Errorf("workspace provider %q liveness check failed (%v), cleanup: %w", prov.ID, aliveErr, err)
	}
	return nil
}

// RunWorkflowCleanup executes the workflow-level cleanup hook. Mirrors
// RunCleanup semantics: missing/cleaned state is skipped, an empty cleanup
// body flips the state to cleaned, errors mark the pseudo-node failed and
// are returned (callers decide fail-fast vs --force).
//
// The hook intentionally never deletes workspace_dir itself — setup/cleanup
// symmetry is the script author's contract ("use an existing directory"
// workflows must stay possible). Outputs survive cleanup (same invariant as
// task cleanup) so a later setup retry can read `prev.*`.
func RunWorkflowCleanup(prov config.WorkspaceProviderConfig, vars effect.WorkflowHookVars, tasks map[string]*contract.TaskState, observer Observer) error {
	obs := observerOr(observer)
	id := contract.WorkflowPseudoNodeID

	state, ok := tasks[id]
	if !ok || state == nil {
		obs.OnSkip(workflowHookScope, id, "no setup state")
		return nil
	}
	if state.Status == contract.TaskStatusCleaned {
		obs.OnSkip(workflowHookScope, id, "already cleaned")
		return nil
	}
	now := time.Now()
	if prov.Cleanup == nil {
		state.Status = contract.TaskStatusCleaned
		state.CleanedAt = now
		obs.OnSuccess(workflowHookScope, id, time.Since(now), nil)
		return nil
	}

	obs.OnStart(workflowHookScope, id)
	eval := effect.ProviderEval(effect.ProviderRoots(vars, nil, state.Outputs, true), vars.Plugins, vars.SourcePath, prov.Ownership())
	_, stderr, runErr := effect.RunProviderAction(prov.Cleanup, eval)
	if runErr != nil {
		state.Status = contract.TaskStatusFailed
		state.Error = runErr.Error()
		state.FailedAt = now
		wrapped := fmt.Errorf("workspace provider %q cleanup: %w", prov.ID, runErr)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return wrapped
	}
	state.Status = contract.TaskStatusCleaned
	state.CleanedAt = now
	state.Error = ""
	obs.OnSuccess(workflowHookScope, id, time.Since(now), stderr)
	return nil
}
