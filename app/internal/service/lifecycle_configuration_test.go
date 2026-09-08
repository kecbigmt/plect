package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

// Given identical parsed configuration, computing the digest twice must
// produce the same value: the projection is a pure function of parsed
// declarations, not of iteration order or wall-clock state.
func TestLifecycleConfigurationDigest_DeterministicForIdenticalConfig(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	session := &domain.Session{Workflow: "coding"}
	plan, err := buildPlanForSession(cfg, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}

	first, err := lifecycleConfigurationDigest(cfg, session, plan)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest: %v", err)
	}
	second, err := lifecycleConfigurationDigest(cfg, session, plan)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest: %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("digest = %q, %q, want two equal non-empty digests", first, second)
	}
}

// A changed setup action must change the digest: it is one of the ADR's
// enumerated projection fields.
func TestLifecycleConfigurationDigest_ChangesWhenSetupActionChanges(t *testing.T) {
	before := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	after := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{"changed":true}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	session := &domain.Session{Workflow: "coding"}

	beforePlan, err := buildPlanForSession(before, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession (before): %v", err)
	}
	beforeDigest, err := lifecycleConfigurationDigest(before, session, beforePlan)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	afterPlan, err := buildPlanForSession(after, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession (after): %v", err)
	}
	afterDigest, err := lifecycleConfigurationDigest(after, session, afterPlan)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (after): %v", err)
	}

	if beforeDigest == afterDigest {
		t.Fatalf("digest unchanged (%q) after the setup action changed", beforeDigest)
	}
}

// Acceptance: "Given first or legacy execution without a baseline, then it
// records one without warning."
func TestUp_FirstExecutionRecordsBaselineWithoutWarning(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})

	result, err := Up(cfg, store, UpParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if result.LifecycleConfigurationWarning != "" {
		t.Errorf("LifecycleConfigurationWarning = %q, want no warning on first execution", result.LifecycleConfigurationWarning)
	}
	s := store.Get("sess-1")
	if s.LifecycleConfigurationDigest == "" {
		t.Error("session baseline was not recorded by the first execution")
	}
}

// Acceptance: "the same configuration does not warn again on the next
// lifecycle operation."
func TestUp_UnchangedConfigurationDoesNotWarnAgain(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Up (first): %v", err)
	}
	baseline := store.Get("sess-1").LifecycleConfigurationDigest

	result, err := Down(cfg, store, DownParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if result.LifecycleConfigurationWarning != "" {
		t.Errorf("LifecycleConfigurationWarning = %q, want no warning against an unchanged baseline", result.LifecycleConfigurationWarning)
	}
	if got := store.Get("sess-1").LifecycleConfigurationDigest; got != baseline {
		t.Errorf("baseline = %q, want it unchanged at %q", got, baseline)
	}
}

// Acceptance: "when up/down/destroy begins, then it warns and executes
// current trusted configuration; the shared baseline advances and the same
// configuration does not warn again on the next lifecycle operation."
func TestUp_ChangedConfigurationWarnsOnceThenCatchesUp(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Up (first): %v", err)
	}
	baseline := store.Get("sess-1").LifecycleConfigurationDigest

	rewriteTaskFixture(t, cfg, taskFixture{id: "build", scope: "run", setup: `echo '{"v":2}'`, cleanup: "true"})

	changed, err := Up(cfg, store, UpParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Up (changed config): %v", err)
	}
	if changed.LifecycleConfigurationWarning == "" {
		t.Fatal("LifecycleConfigurationWarning = \"\", want a warning after the setup action changed")
	}
	advanced := store.Get("sess-1").LifecycleConfigurationDigest
	if advanced == baseline {
		t.Fatal("baseline did not advance to the new digest")
	}

	caughtUp, err := Down(cfg, store, DownParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Down (same config as last execution): %v", err)
	}
	if caughtUp.LifecycleConfigurationWarning != "" {
		t.Errorf("LifecycleConfigurationWarning = %q, want no warning once the baseline has caught up", caughtUp.LifecycleConfigurationWarning)
	}
}

// Acceptance: "Given cleanup starts and fails, then the obligation remains
// outstanding" -- and per the ADR, "An execution failure does not erase
// that fact" about the baseline having advanced.
func TestUp_ExecutionFailureStillAdvancesBaseline(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "flaky", scope: "run", setup: `exit 1`, cleanup: "true"}},
		[]nodeFixture{{id: "flaky"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err == nil {
		t.Fatal("Up: want the flaky setup's failure surfaced, got nil error")
	}
	if got := store.Get("sess-1").LifecycleConfigurationDigest; got == "" {
		t.Error("baseline was not recorded despite the execution failure")
	}
}

// An outstanding execution whose node has left the desired workflow keeps
// its current trusted cleanup definition in the comparison via its own
// retained declaration identity. Repairing that definition, with the
// workflow left unchanged, still warns and the next teardown runs the
// repaired definition.
func TestDown_RepairedCleanupForNodeOutsideWorkflowWarnsAndRuns(t *testing.T) {
	requireBash(t)
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	// "retired" is never in the workflow's own node list: its outstanding
	// execution is tracked purely through session.Nodes, which is exactly
	// the "node no longer in the desired workflow" case. "kept" only exists
	// so the workflow declares at least one node.
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "kept", scope: "run", setup: `echo '{}'`, cleanup: "true"},
			{id: "retired", scope: "run", cleanup: "exit 1"},
		},
		[]nodeFixture{{id: "kept"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{
		"kept":    {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "kept", Seq: 1, Outputs: map[string]any{}},
		"retired": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "retired", Seq: 2, Outputs: map[string]any{}},
	})

	if _, err := Down(cfg, store, DownParams{Identifier: "sess-1"}); err == nil {
		t.Fatal("Down (first, failing cleanup): want the cleanup failure surfaced, got nil error")
	}
	baseline := store.Get("sess-1").LifecycleConfigurationDigest
	if baseline == "" {
		t.Fatal("baseline was not recorded by the first (failing) execution")
	}
	if s := store.Get("sess-1").Nodes["retired"]; s == nil || s.Status == contract.TaskStatusCleaned {
		t.Fatalf("retired = %+v, want it left unreleased after the failing cleanup", s)
	}

	rewriteTaskFixture(t, cfg, taskFixture{id: "retired", scope: "run", cleanup: "printf 'retired-cleaned\\n' >> " + cleanupLog})

	repaired, err := Down(cfg, store, DownParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Down (repaired cleanup): %v", err)
	}
	if repaired.LifecycleConfigurationWarning == "" {
		t.Fatal("LifecycleConfigurationWarning = \"\", want a warning after the retained cleanup definition was repaired")
	}
	if got := store.Get("sess-1").LifecycleConfigurationDigest; got == baseline {
		t.Fatal("baseline did not advance after the repaired cleanup ran")
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil || string(data) != "retired-cleaned\n" {
		t.Fatalf("cleanup log = %q, %v, want the repaired cleanup to have run", data, err)
	}
	if s := store.Get("sess-1").Nodes["retired"]; s == nil || s.Status != contract.TaskStatusCleaned {
		t.Errorf("retired = %+v, want it released (cleaned) after the repaired cleanup succeeded", s)
	}
}

// rewriteTaskFixture overwrites one task definition file written by an
// earlier writeWorkflowFixture call, so a test can change lifecycle
// configuration between two lifecycle operations against the same session
// without re-seeding it under a new workspace.
func rewriteTaskFixture(t *testing.T, cfg *config.Config, d taskFixture) {
	t.Helper()
	path := filepath.Join(cfg.BaseDir, "tasks", d.id+".toml")
	if err := os.WriteFile(path, []byte(effectFixtureDoc(d)), 0o644); err != nil {
		t.Fatalf("rewrite task fixture %q: %v", d.id, err)
	}
}
