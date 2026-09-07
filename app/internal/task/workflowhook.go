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
// pseudo-node. Semantics mirror RunSetup: idempotent (an already-produced
// pseudo-node is skipped), `prev.*` carries the prior outputs across retries,
// stdout is the JSON outputs contract.
//
// tasks must combine every collection Seq is shared across (a session's Nodes
// and Tasks) — see domain.MergedTasks — since the pseudo-node's Seq is stamped
// from the highest Seq found in it; the caller then persists the result key
// (contract.WorkflowPseudoNodeID) into session.Nodes, the collection the
// pseudo-node actually belongs to.
//
// Additional contract: the outputs MUST contain the reserved `workspace_dir`
// key (non-empty string) — every downstream consumer (cascade resolution,
// task cwd, cd/attach) depends on it. The outputs are validated against the
// workflow's outputs schema when one is declared.
//
// Returns the pseudo-node outputs (whether fresh or reused).
func RunWorkflowSetup(prov config.WorkspaceProviderConfig, vars effect.WorkflowHookVars, tasks map[string]*contract.TaskState, observer Observer) (map[string]any, error) {
	obs := observerOr(observer)
	id := contract.WorkflowPseudoNodeID

	if existing, ok := tasks[id]; ok && existing != nil && existing.Status == contract.TaskStatusProduced {
		obs.OnSkip(workflowHookScope, id, "already produced")
		return existing.Outputs, nil
	}
	if prov.Setup == nil {
		return nil, fmt.Errorf("workspace provider %q declares no setup", prov.ID)
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
		return nil, wrapped
	}
	outputs, parseErr := lang.ParseOutputs(stdout)
	if parseErr != nil {
		fail(parseErr.Error())
		wrapped := fmt.Errorf("workspace provider %q setup: %w", prov.ID, parseErr)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return nil, wrapped
	}
	workspaceDir, _ := outputs[contract.OutputKeyWorkspaceDir].(string)
	if strings.TrimSpace(workspaceDir) == "" {
		msg := fmt.Sprintf("setup outputs must contain a non-empty %q string (got %v)", contract.OutputKeyWorkspaceDir, outputs[contract.OutputKeyWorkspaceDir])
		fail(msg)
		wrapped := fmt.Errorf("workspace provider %q setup: %s", prov.ID, msg)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return nil, wrapped
	}
	schema, err := lang.CompileSchema(prov.OutputsSchema, prov.ResolvedOutputsSchemaPath(), "plect:workspace_provider:"+prov.ID+":outputs")
	if err != nil {
		fail(err.Error())
		wrapped := fmt.Errorf("workspace provider %q outputs schema: %w", prov.ID, err)
		obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
		return nil, wrapped
	}
	if schema != nil {
		if vErr := schema.Validate(outputs); vErr != nil {
			fail(vErr.Error())
			wrapped := fmt.Errorf("workspace provider %q setup: outputs schema: %w", prov.ID, vErr)
			obs.OnFailure(workflowHookScope, id, time.Since(now), wrapped, stderr)
			return nil, wrapped
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
	return outputs, nil
}

// RunWorkflowCleanup executes the workflow-level cleanup hook. Mirrors
// RunCleanup semantics: missing/cleaned state is skipped, an empty cleanup
// body flips the state to cleaned, errors mark the pseudo-node failed and
// are returned (callers decide fail-fast vs --force). Mutation is through the
// pointer already stored in tasks, so a caller passing session.Nodes directly
// (the pseudo-node's own collection) needs no write-back.
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
