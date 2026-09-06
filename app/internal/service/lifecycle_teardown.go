package service

import (
	"fmt"
	"sort"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// runWorkflowCleanupForDestroy resolves the session's workflow definition and
// runs its cleanup hook. The definition comes from the trusted layers (the
// workspace-dir layer cannot declare hooks), so resolving against the
// session's workspace directory path is safe even though that path is clone
// content.
func runWorkflowCleanupForDestroy(cfg *config.Config, session *domain.Session, force bool, cleanupInputs map[string]string, observer task.Observer) error {
	workflows, err := cfg.LoadWorkflows(session.WorkspaceDirPath)
	if err != nil {
		return fmt.Errorf("load workflows: %w", err)
	}
	wf, ok := workflows[session.Workflow]
	if !ok {
		return fmt.Errorf("workflow %q not found; its cleanup hook cannot run", session.Workflow)
	}
	workspaceProviders, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		return fmt.Errorf("load workspace providers: %w", err)
	}
	prov, ok, provErr := workspaceProviderFor(wf, workspaceProviders)
	if provErr != nil {
		return provErr
	}
	if !ok {
		return fmt.Errorf("workflow %q declares no workspace provider; its cleanup hook cannot run", session.Workflow)
	}
	provInputs, inputsErr := resolveWorkspaceProviderInputs(prov, wf)
	if inputsErr != nil {
		return inputsErr
	}
	vars := effect.WorkflowHookVars{
		ResourceID:        session.ResourceID,
		SessionName:       session.Name,
		WorkspaceDirsRoot: cfg.WorkspaceDirsRoot,
		SessionInputs:     session.Inputs,
		Inputs:            provInputs,
		Plugins:           cfg.Plugins,
		SourcePath:        prov.SourcePath,
		Force:             force,
		CleanupInputs:     cleanupInputs,
	}
	return task.RunWorkflowCleanup(prov, vars, session.Tasks, observer)
}

// unifiedTeardownList builds the single cleanup-ordered Resolved list for a
// teardown phase: static plan nodes and dynamic instances merged into one
// slice sorted by ascending instantiation Seq. RunCleanup reclaims in
// reverse, so the result is strictly the reverse of the instantiation stack —
// a static node instantiated after a dynamic one (e.g. a re-`up` that re-stamps
// run nodes) is still cleaned first. The @workflow pseudo-node is excluded; it
// is released last via the workspace provider cleanup hook.
//
// runOnly restricts to run-scoped tasks (the `down` lifecycle); destroy
// passes false to reclaim every task regardless of scope. Static nodes are
// enumerated session-then-run so legacy state (Seq all zero) preserves the old
// run-before-session reverse order through the stable sort. A dynamic instance
// whose task definition has since disappeared is reclaimed with an empty
// cleanup (best-effort, since there is no definition left to run against).
func unifiedTeardownList(cfg *config.Config, session *domain.Session, plan *task.Plan, runOnly bool) ([]task.Resolved, error) {
	type seqResolved struct {
		seq int
		r   task.Resolved
	}
	var items []seqResolved
	static := make(map[string]bool)

	appendStatic := func(nodes []task.Resolved) {
		for _, r := range nodes {
			if runOnly && r.Scope != contract.TaskScopeRun {
				continue
			}
			seq := 0
			if st := session.Tasks[r.NodeID]; st != nil {
				// A `--name` collides only against existing state, so an
				// uninstantiated node leaves its id free for a dynamic
				// instance to take. What the key holds is then that instance,
				// not this node, and tearing it down as the node would run a
				// cleanup belonging to another declaration entirely.
				if st.Dynamic {
					continue
				}
				seq = st.Seq
			}
			items = append(items, seqResolved{seq: seq, r: r})
			static[r.NodeID] = true
		}
	}
	appendStatic(plan.Session)
	appendStatic(plan.Run)

	defs, err := cfg.LoadTaskDefinitions(session.WorkspaceDirPath)
	if err != nil {
		return nil, fmt.Errorf("load task definitions: %w", err)
	}
	// Sort dynamic keys for a deterministic input order before the stable sort
	// (map iteration is random; equal-seq legacy entries would otherwise vary).
	dynKeys := make([]string, 0, len(session.Tasks))
	for key, st := range session.Tasks {
		if st == nil || !st.Dynamic || key == contract.WorkflowPseudoNodeID || static[key] {
			continue
		}
		if runOnly && st.Scope != contract.TaskScopeRun {
			continue
		}
		dynKeys = append(dynKeys, key)
	}
	sort.Strings(dynKeys)
	nodes := nodeAddresses(cfg, session)
	for _, key := range dynKeys {
		st := session.Tasks[key]
		taskID := instanceDefinitionAddress(key, st, nodes)
		// Build only the cleanup-relevant fields straight from the definition —
		// no schema / requires / done_when validation (that runs at create / up /
		// task run). Teardown must stay resilient to a def whose config drifted
		// to invalid after the instance was created: a present-but-invalid def
		// must be no more fatal than a disappeared one, so `plect destroy --force`
		// can still reclaim the session. Cleanup needs only the script plus the
		// persisted inputs/outputs.
		r := task.Resolved{NodeID: key, TaskID: taskID, Scope: st.Scope}
		if def, ok := defs[taskID]; ok {
			r.Cleanup = def.Cleanup
			r.SourcePath = def.SourcePath
			r.Layers = effect.CleanupLayers(def)
		}
		items = append(items, seqResolved{seq: st.Seq, r: r})
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].seq < items[j].seq })
	out := make([]task.Resolved, len(items))
	for i, it := range items {
		out[i] = it.r
	}
	return out, nil
}
