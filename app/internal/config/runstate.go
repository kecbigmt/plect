package config

import (
	"fmt"
	"sync"

	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// ResolveSessionWorkflow loads the session's frozen workflow, distinguishing
// "no workflow declared" (nil, nil — a manually created or legacy session
// has no current plan to evaluate) from "a workflow is declared but no
// longer resolves" (an error) — the latter is a per-session plan-resolution
// failure, e.g. the workflow was renamed or removed after the session froze
// it.
func (c *Config) ResolveSessionWorkflow(s *domain.Session) (*WorkflowFile, error) {
	if s.Workflow == "" {
		return nil, nil
	}
	workflows, err := c.LoadWorkflows(s.WorkspaceDirPath)
	if err != nil {
		return nil, err
	}
	wf, ok := workflows[s.Workflow]
	if !ok {
		return nil, fmt.Errorf("workflow %q not found", s.Workflow)
	}
	return &wf, nil
}

// runScopeCache memoizes CurrentPlanRunScopedNodeSet by (workflow,
// workspaceDirPath), reached via a pointer field (not embedded) since Config
// is copied by value in test fixture tables and an embedded sync.Mutex would
// make each copy a lock copy.
type runScopeCache struct {
	mu    sync.Mutex
	byKey map[runScopeCacheKey]runScopeCacheEntry
}

type runScopeCacheKey struct {
	workflow         string
	workspaceDirPath string
}

type runScopeCacheEntry struct {
	set map[string]bool
	ok  bool
}

func (rc *runScopeCache) get(key runScopeCacheKey) (runScopeCacheEntry, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, ok := rc.byKey[key]
	return e, ok
}

func (rc *runScopeCache) put(key runScopeCacheKey, entry runScopeCacheEntry) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.byKey == nil {
		rc.byKey = make(map[runScopeCacheKey]runScopeCacheEntry)
	}
	rc.byKey[key] = entry
}

// runScopeCacheInstance lazily creates c's cache instance via CompareAndSwap, so concurrent first callers race safely.
func (c *Config) runScopeCacheInstance() *runScopeCache {
	if rc := c.runScopeCache.Load(); rc != nil {
		return rc
	}
	rc := &runScopeCache{}
	if !c.runScopeCache.CompareAndSwap(nil, rc) {
		rc = c.runScopeCache.Load()
	}
	return rc
}

// CurrentPlanRunScopedNodeSet resolves s's frozen workflow's declared
// run-scoped node ids as a set. ok is false when the workflow or its task
// definitions do not resolve at all, which a caller must tell apart from a
// legitimately empty node set (ok=true).
//
// Memoized per (workflow, workspaceDirPath) for this *Config's lifetime:
// dispatch.Supervisor and reactor.Supervisor each re-evaluate RunScopeUp for
// every up session on every ~1s poll tick, and each evaluation used to
// re-parse every definition file on disk from scratch just to answer this.
// A new *Config (config.Live's own refresh) is what invalidates the cache —
// this must not be used where a caller needs to see an on-disk edit sooner
// than that (workflow/task-definition loading elsewhere in this package is
// unaffected and stays uncached).
func (c *Config) CurrentPlanRunScopedNodeSet(s *domain.Session) (set map[string]bool, ok bool) {
	key := runScopeCacheKey{workflow: s.Workflow, workspaceDirPath: s.WorkspaceDirPath}
	cache := c.runScopeCacheInstance()
	if entry, hit := cache.get(key); hit {
		return entry.set, entry.ok
	}
	set, ok = c.resolveCurrentPlanRunScopedNodeSet(s)
	cache.put(key, runScopeCacheEntry{set: set, ok: ok})
	return set, ok
}

func (c *Config) resolveCurrentPlanRunScopedNodeSet(s *domain.Session) (set map[string]bool, ok bool) {
	wf, err := c.ResolveSessionWorkflow(s)
	if err != nil || wf == nil {
		return nil, false
	}
	defs, err := c.LoadTaskDefinitions(s.WorkspaceDirPath)
	if err != nil {
		return nil, false
	}
	out := make(map[string]bool, len(wf.Nodes))
	for _, n := range wf.Nodes {
		if n.Uses == "" {
			continue
		}
		def, defOK := defs[n.Uses]
		if !defOK || def.Scope != TaskScopeRun {
			continue
		}
		out[n.ID] = true
	}
	return out, true
}

// RunScopeUp reports whether the session's current plan has a produced
// run-scoped node — the one authority every consumer of the run fact (CLI
// status, the reactor/dispatch supervisors, population capacity) shares, so
// a task-state entry for a node the workflow no longer declares cannot read
// a session as up in one place while another consumer still treats it as
// down. Falls back to any produced run-scoped task-state entry at all when
// the workflow itself does not resolve, so an unrelated config problem does
// not misreport an otherwise-live session as down.
func (c *Config) RunScopeUp(s *domain.Session) bool {
	if s == nil {
		return false
	}
	current, ok := c.CurrentPlanRunScopedNodeSet(s)
	if !ok {
		return anyProducedRunScopedTaskState(domain.MergedTasks(s))
	}
	for id := range current {
		st := s.Nodes[id]
		if st != nil && st.Scope == TaskScopeRun && st.Status == contract.TaskStatusProduced {
			return true
		}
	}
	return false
}

// anyProducedRunScopedTaskState is RunScopeUp's fallback for a session whose
// workflow does not resolve: any produced run-scoped task-state entry at
// all, regardless of whether its node is still declared.
func anyProducedRunScopedTaskState(tasks map[string]*contract.TaskState) bool {
	for _, e := range tasks {
		if e != nil && e.Scope == TaskScopeRun && e.Status == contract.TaskStatusProduced {
			return true
		}
	}
	return false
}
