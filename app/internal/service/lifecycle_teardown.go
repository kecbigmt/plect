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
	return task.RunWorkflowCleanup(prov, vars, session.Nodes, observer)
}

// teardownItem pairs one teardown-eligible node or dynamic instance with its
// own instantiation Seq, the fallback tie-break orderTeardownItems uses.
type teardownItem struct {
	seq int
	r   task.Resolved
}

// unifiedTeardownList builds the single cleanup-ordered Resolved list for a
// teardown phase, merging every retained unreleased node execution with
// every dynamic instance (RunCleanup reclaims in reverse, so the result is
// ordered prerequisites-before-dependents; see orderTeardownItems). The
// @workflow pseudo-node is excluded; it releases last via the workspace
// provider cleanup hook. Static nodes are enumerated from session.Nodes, not
// plan, so a node the current workflow no longer declares is still torn
// down using what that execution itself retained (resolveNodeCleanup).
// runOnly restricts to run-scoped tasks (the `down` lifecycle); destroy
// passes false for every scope.
func unifiedTeardownList(cfg *config.Config, session *domain.Session, runOnly bool) ([]task.Resolved, error) {
	defs, err := cfg.LoadTaskDefinitions(session.WorkspaceDirPath)
	if err != nil {
		return nil, fmt.Errorf("load task definitions: %w", err)
	}
	nodes := nodeAddresses(cfg, session)

	var items []teardownItem
	static := make(map[string]bool, len(session.Nodes))

	for _, key := range sortedTaskKeys(session.Nodes) {
		if key == contract.WorkflowPseudoNodeID {
			continue
		}
		st := session.Nodes[key]
		if st == nil || st.Status == contract.TaskStatusCleaned {
			continue
		}
		if session.Tasks[key] != nil {
			// A `--name` collides only against existing state, so an
			// uninstantiated node leaves its id free for a dynamic
			// instance to take. What the key holds is then that instance,
			// not this node, and tearing it down as the node would run a
			// cleanup belonging to another declaration entirely -- so this
			// key is deliberately NOT marked static, letting the dynamic
			// branch below enumerate it as that instance instead.
			continue
		}
		if runOnly && st.Scope != contract.TaskScopeRun {
			continue
		}
		taskID := instanceDefinitionAddress(key, st, true, nodes)
		r := task.Resolved{NodeID: key, TaskID: taskID, Scope: st.Scope, DependsOn: st.DependsOn}
		resolveNodeCleanup(&r, st, defs)
		items = append(items, teardownItem{seq: st.Seq, r: r})
		static[key] = true
	}

	// Sort dynamic keys for a deterministic input order before ordering
	// (map iteration is random; equal-seq legacy entries would otherwise vary).
	dynKeys := make([]string, 0, len(session.Tasks))
	for key, st := range session.Tasks {
		if st == nil || static[key] {
			continue
		}
		if runOnly && st.Scope != contract.TaskScopeRun {
			continue
		}
		dynKeys = append(dynKeys, key)
	}
	sort.Strings(dynKeys)
	for _, key := range dynKeys {
		st := session.Tasks[key]
		taskID := instanceDefinitionAddress(key, st, true, nodes)
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
		items = append(items, teardownItem{seq: st.Seq, r: r})
	}

	return orderTeardownItems(items), nil
}

// resolveNodeCleanup fills r's cleanup fields from st's own retained
// contract when one exists (task.DecodeRetainedCleanup or
// effect.LayersFromRetained), falling back to re-resolving r.TaskID against
// defs -- tolerant of a missing/drifted definition, like a dynamic
// instance's teardown -- only when st retains nothing usable.
func resolveNodeCleanup(r *task.Resolved, st *contract.TaskState, defs map[string]config.TaskDefinition) {
	if rc, ok, err := task.DecodeRetainedCleanup(st.Cleanup); err == nil && ok {
		r.Cleanup = rc.Action
		r.SourcePath = rc.SourcePath
		r.From = rc.From
		return
	}
	if layers, ok := effect.LayersFromRetained(st.Layers); ok {
		r.Layers = layers
		return
	}
	if def, ok := defs[r.TaskID]; ok {
		r.Cleanup = def.Cleanup
		r.SourcePath = def.SourcePath
		r.Layers = effect.CleanupLayers(def)
	}
}

// orderTeardownItems orders each item's dependent before its prerequisite
// (per DependsOn), so RunCleanup's reverse iteration releases the dependent
// first. Ties and unrecorded edges fall back to ascending Seq; a cycle
// falls back to plain Seq order for the whole list.
func orderTeardownItems(items []teardownItem) []task.Resolved {
	n := len(items)
	if n == 0 {
		return nil
	}
	indexByKey := make(map[string]int, n)
	for i, it := range items {
		indexByKey[it.r.NodeID] = i
	}
	inDegree := make([]int, n)
	dependents := make([][]int, n)
	for i, it := range items {
		for _, dep := range it.r.DependsOn {
			j, ok := indexByKey[dep]
			if !ok {
				// The dependency has no entry in this same teardown pass
				// (already released, or never existed) -- nothing left to
				// order this item against.
				continue
			}
			inDegree[i]++
			dependents[j] = append(dependents[j], i)
		}
	}

	bySeq := func(idx []int) {
		sort.SliceStable(idx, func(a, b int) bool { return items[idx[a]].seq < items[idx[b]].seq })
	}
	var ready []int
	for i := range items {
		if inDegree[i] == 0 {
			ready = append(ready, i)
		}
	}
	order := make([]int, 0, n)
	for len(ready) > 0 {
		bySeq(ready)
		i := ready[0]
		ready = ready[1:]
		order = append(order, i)
		for _, d := range dependents[i] {
			inDegree[d]--
			if inDegree[d] == 0 {
				ready = append(ready, d)
			}
		}
	}

	out := make([]task.Resolved, n)
	if len(order) != n {
		sorted := append([]teardownItem(nil), items...)
		sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].seq < sorted[b].seq })
		for i, it := range sorted {
			out[i] = it.r
		}
		return out
	}
	for i, idx := range order {
		out[i] = items[idx].r
	}
	return out
}
