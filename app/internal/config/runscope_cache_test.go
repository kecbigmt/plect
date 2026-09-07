package config

import (
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/domain"
)

// The reconcile hot path (dispatch/reactor Supervisor.reconcile -> RunScopeUp
// -> CurrentPlanRunScopedNodeSet) calls this once per up session per ~1s poll
// tick, so a second call for the same (workflow, workspaceDirPath) must reuse
// the first's result rather than re-parsing — proven here by rewriting the
// workflow file between two calls on the same *Config and checking the
// second call still sees the pre-edit node set.
func TestCurrentPlanRunScopedNodeSet_CachesPerConfigLifetime(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	cfg := &Config{BaseDir: base}
	s := &domain.Session{Workflow: "default", WorkspaceDirPath: ""}

	first, ok := cfg.CurrentPlanRunScopedNodeSet(s)
	if !ok || len(first) != 2 {
		t.Fatalf("first call = %+v, ok=%v, want the two-node fixture set", first, ok)
	}

	writeFile(t, filepath.Join(base, "workflows", "default.toml"), `
[default]
kind = "workflow"

[[default.nodes]]
id   = "pane"
uses = "pane"

[[default.nodes]]
id   = "agent"
uses = "agent"

[[default.nodes]]
id   = "extra"
uses = "agent"
`)

	again, ok := cfg.CurrentPlanRunScopedNodeSet(s)
	if !ok || len(again) != 2 {
		t.Errorf("second call on the same *Config = %+v, ok=%v, want the cached two-node set unchanged by the on-disk edit", again, ok)
	}
}

// A new *Config (config.Live's own periodic swap in production) is what
// invalidates the cache: the same edit as above, read through a fresh
// *Config, must be seen immediately.
func TestCurrentPlanRunScopedNodeSet_NewConfigSeesEdit(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	s := &domain.Session{Workflow: "default", WorkspaceDirPath: ""}

	if _, ok := (&Config{BaseDir: base}).CurrentPlanRunScopedNodeSet(s); !ok {
		t.Fatal("priming call: ok = false")
	}

	writeFile(t, filepath.Join(base, "workflows", "default.toml"), `
[default]
kind = "workflow"

[[default.nodes]]
id   = "pane"
uses = "pane"

[[default.nodes]]
id   = "agent"
uses = "agent"

[[default.nodes]]
id   = "extra"
uses = "agent"
`)

	fresh, ok := (&Config{BaseDir: base}).CurrentPlanRunScopedNodeSet(s)
	if !ok || len(fresh) != 3 {
		t.Errorf("call on a fresh *Config = %+v, ok=%v, want the edited three-node set", fresh, ok)
	}
}

// Two sessions with different workspaceDirPath must not collide on one cache
// entry. workspaceB carries its own `.plect/workflows/` overlay appending a
// third run-scoped node to "default", so the two keys' correct answers
// genuinely differ (2 nodes vs. 3) — a cache that conflated the two keys
// would return the wrong count for at least one of them, which a same-length
// assertion could not have caught.
func TestCurrentPlanRunScopedNodeSet_CachesPerWorkspaceDirPathIndependently(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	cfg := &Config{BaseDir: base}

	workspaceB := t.TempDir()
	writeFile(t, filepath.Join(workspaceB, ".plect", "workflows", "default.toml"), `
[default]
kind = "workflow"

[[default.nodes]]
id   = "extra"
uses = "agent"
`)

	a, ok := cfg.CurrentPlanRunScopedNodeSet(&domain.Session{Workflow: "default", WorkspaceDirPath: ""})
	if !ok || len(a) != 2 {
		t.Fatalf("workspace a = %+v, ok=%v, want the global-layer-only two-node set", a, ok)
	}
	b, ok := cfg.CurrentPlanRunScopedNodeSet(&domain.Session{Workflow: "default", WorkspaceDirPath: workspaceB})
	if !ok || len(b) != 3 || !b["extra"] {
		t.Fatalf("workspace b = %+v, ok=%v, want the overlay's three-node set including \"extra\"", b, ok)
	}

	// Repeat a: must still be the cached, correct two-node answer, not
	// disturbed by (or leaking into) workspace b's three-node one.
	again, ok := cfg.CurrentPlanRunScopedNodeSet(&domain.Session{Workflow: "default", WorkspaceDirPath: ""})
	if !ok || len(again) != 2 || again["extra"] {
		t.Errorf("repeat workspace a = %+v, ok=%v, want the unchanged two-node set", again, ok)
	}
}
