package config

import (
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

// countingDiscoverLayer wraps discoverLayer with an invocation counter,
// matching reactor.Supervisor's deadmanFn/tickFn test seam convention.
func countingDiscoverLayer(calls *int) func(layerDir) ([]*lang.Definition, error) {
	return func(l layerDir) ([]*lang.Definition, error) {
		*calls++
		return discoverLayer(l)
	}
}

// The reconcile hot path (dispatch/reactor Supervisor.reconcile -> RunScopeUp
// -> discoverLayers) calls this once per up session per ~1s poll tick, so the
// call count below must stay flat as passes grow, not scale with them.
func TestDiscoverLayers_CachesRepeatedCallsWithSameWorkspaceDirPath(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	var calls int
	cfg := &Config{BaseDir: base, discoverLayerFn: countingDiscoverLayer(&calls)}

	const passes = 25
	for i := 0; i < passes; i++ {
		if _, err := cfg.discoverLayers(""); err != nil {
			t.Fatalf("pass %d: discoverLayers: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("discoverLayer invocations across %d passes = %d, want 1 (result must be cached, not re-parsed every pass)", passes, calls)
	}
}

// Two sessions with different workspaceDirPath must not collide on one
// cache entry, and each is still only computed once no matter how often
// it's asked for again.
func TestDiscoverLayers_CachesPerWorkspaceDirPathIndependently(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	var calls int
	cfg := &Config{BaseDir: base, discoverLayerFn: countingDiscoverLayer(&calls)}

	first, err := cfg.discoverLayers("")
	if err != nil {
		t.Fatalf("discoverLayers(a): %v", err)
	}
	callsAfterFirstKey := calls

	workspaceB := t.TempDir()
	if _, err := cfg.discoverLayers(workspaceB); err != nil {
		t.Fatalf("discoverLayers(b): %v", err)
	}
	if calls == callsAfterFirstKey {
		t.Fatalf("a second, distinct workspaceDirPath was answered from the first key's cache entry instead of computing its own")
	}
	callsAfterSecondKey := calls

	for i := 0; i < 10; i++ {
		if _, err := cfg.discoverLayers(""); err != nil {
			t.Fatalf("repeat discoverLayers(a) #%d: %v", i, err)
		}
		if _, err := cfg.discoverLayers(workspaceB); err != nil {
			t.Fatalf("repeat discoverLayers(b) #%d: %v", i, err)
		}
	}
	if calls != callsAfterSecondKey {
		t.Errorf("discoverLayer invocations after repeats = %d, want %d (both keys already cached)", calls, callsAfterSecondKey)
	}

	again, err := cfg.discoverLayers("")
	if err != nil {
		t.Fatalf("discoverLayers(a) again: %v", err)
	}
	if len(again) != len(first) {
		t.Errorf("cached result for the same key changed shape: got %d layers, want %d", len(again), len(first))
	}
}

// Config-level half of the regression dispatch.TestSupervisor_ValidationRecoveryOnNextUpClearsStreak
// and reactor.TestRefreshTickConfig_KeepsAndAdopts cover end-to-end.
func TestLoadWorkflowsFresh_SeesAnEditTheCachedReadWouldMiss(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	cfg := &Config{BaseDir: base}

	if _, err := cfg.LoadWorkflows(""); err != nil {
		t.Fatalf("prime the cache: %v", err)
	}

	writeFile(t, filepath.Join(base, "workflows", "default.toml"), `
[default]
kind = "workflow"
name = "renamed"

[[default.nodes]]
id   = "pane"
uses = "pane"

[[default.nodes]]
id   = "agent"
uses = "agent"
`)

	stale, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	if got := stale["default"].Name; got != "" {
		t.Fatalf("LoadWorkflows after an on-disk edit = name %q, want the cached (pre-edit) empty name — this call was never meant to see the edit", got)
	}

	fresh, err := cfg.LoadWorkflowsFresh("")
	if err != nil {
		t.Fatalf("LoadWorkflowsFresh: %v", err)
	}
	if got := fresh["default"].Name; got != "renamed" {
		t.Errorf("LoadWorkflowsFresh name = %q, want %q (the edit written after the cache was primed)", got, "renamed")
	}

	// The fresh read repopulates the cache: a later plain LoadWorkflows must
	// now see the same edit, not fall back to what was cached before.
	again, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows after fresh: %v", err)
	}
	if got := again["default"].Name; got != "renamed" {
		t.Errorf("LoadWorkflows after LoadWorkflowsFresh = name %q, want %q (fresh must repopulate the cache)", got, "renamed")
	}
}
