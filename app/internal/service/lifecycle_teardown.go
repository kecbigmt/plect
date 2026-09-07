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

// unifiedTeardownList merges every unreleased node execution with every
// dynamic instance into one dependency-ordered Resolved list (see
// orderTeardownItems); the @workflow pseudo-node releases last, via the
// workspace provider cleanup hook. Static nodes are enumerated from
// session.Nodes, not plan, so a node the current workflow no longer
// declares is still torn down (resolveNodeCleanup).
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
			// key holds a dynamic instance, not this node; deliberately not
			// marked static, so the dynamic branch below enumerates it.
			continue
		}
		if runOnly && st.Scope != contract.TaskScopeRun {
			continue
		}
		taskID := instanceDefinitionAddress(key, st, true, nodes)
		r := task.Resolved{NodeID: key, TaskID: taskID, Scope: st.Scope, DependsOn: st.DependsOn}
		r.Unresolved = !resolveNodeCleanup(&r, defs)
		items = append(items, teardownItem{seq: st.Seq, r: r})
		static[key] = true
	}

	// Deterministic input order; map iteration is random.
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
		// Only the cleanup-relevant fields, so a def drifted invalid since
		// creation is no more fatal than a disappeared one.
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

// resolveNodeCleanup resolves r's cleanup recipe fresh from config by
// r.TaskID. ok is false when no such definition exists any more.
func resolveNodeCleanup(r *task.Resolved, defs map[string]config.TaskDefinition) bool {
	def, ok := defs[r.TaskID]
	if !ok {
		return false
	}
	r.Cleanup = def.Cleanup
	r.SourcePath = def.SourcePath
	r.Layers = effect.CleanupLayers(def)
	return true
}

// orderTeardownItems orders each dependent before its prerequisite (per
// DependsOn); ties, unrecorded edges, and any cycle fall back to Seq.
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
