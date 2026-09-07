package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contract "github.com/kecbigmt/plecture/contracts/state"
)

// FinalizeTask must refuse — no cleanup attempted, instance left in place —
// when done_when is not currently satisfied. Finalization must never be the
// thing that forces completion (ADR "goal-as-task" D4).
func TestFinalizeTask_RefusesWhenNotSatisfied(t *testing.T) {
	cfg := checkStatusOnlyConfig(t, 0)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "work",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"checks_status": "PENDING"}),
		},
	})

	if _, err := FinalizeTask(cfg, store, FinalizeTaskParams{Instance: "initial", SessionName: "o/r-1"}); err == nil {
		t.Fatal("expected an error when done_when is not satisfied")
	}
	if store.Get("o/r-1").Tasks["initial"] == nil {
		t.Error("instance must not be reclaimed when finalize refuses")
	}
}

// Satisfied + no bound resource: finalize records completion and leaves the
// instance in place — cleanup is a separate, explicit step.
func TestFinalizeTask_SatisfiedNoResourceLeavesInstanceForCleanup(t *testing.T) {
	cfg := checkStatusOnlyConfig(t, 0)
	// finalize re-observes before it reconfirms, so the observation has to
	// report the facts the predicate reads rather than the seeded snapshot.
	stubObservedFacts(t, cfg, ".", map[string]any{"checks_status": "SUCCESS"})
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "work",
			Status:   contract.TaskStatusProduced,
			Observed: observedFacts(map[string]any{"checks_status": "SUCCESS"}),
		},
	})

	result, err := FinalizeTask(cfg, store, FinalizeTaskParams{Instance: "initial", SessionName: "o/r-1"})
	if err != nil {
		t.Fatalf("FinalizeTask: %v", err)
	}
	if result.Finalized {
		t.Error("Finalized = true, want false with no bound resource")
	}
	st := store.Get("o/r-1").Tasks["initial"]
	if st == nil {
		t.Fatal("instance must not be reclaimed by finalize — cleanup is a separate step")
	}
	if st.FinalizedAt.IsZero() {
		t.Error("FinalizedAt must be recorded")
	}

	cleanup, cerr := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"})
	if cerr != nil {
		t.Fatalf("TaskCleanup after finalize: %v", cerr)
	}
	if !cleanup.Found {
		t.Error("cleanup after finalize must find and reclaim the instance")
	}
	if store.Get("o/r-1").Tasks["initial"] != nil {
		t.Error("instance must be reclaimed after the explicit cleanup call")
	}
}

// Satisfied + a bound resource whose observer declares a finalize action:
// the action runs (seeing the resource and revision as evidence); the
// instance itself is left in place for a separate `plect task cleanup`.
func TestFinalizeTask_RunsResourceFinalizeAndLeavesInstance(t *testing.T) {
	cfg := checkStatusOnlyConfig(t, 0)
	out := filepath.Join(t.TempDir(), "finalize.out")
	resourcesDir := filepath.Join(cfg.BaseDir, "resources")
	if err := os.MkdirAll(resourcesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf(`
[local_okf]
kind  = "resource_observer"
match = '^local-okf://'

[local_okf.observe]
type    = "exec"
command = "true"

[local_okf.finalize]
type    = "exec"
command = "sh"
args    = ["-c", 'echo "$1 $2" > %s', "finalize", { from = "resource.id" }, { from = "resource.revision" }]
`, out)
	if err := os.WriteFile(filepath.Join(resourcesDir, "local-okf.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Narrow enough to leave the local-okf resource to the observer this test
	// declares for it, which is the one finalize has to run.
	stubObservedFacts(t, cfg, "^https://", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "work",
			Status:   contract.TaskStatusProduced,
			Resource: "local-okf://kec/goals/x.md",
		},
	})

	result, err := FinalizeTask(cfg, store, FinalizeTaskParams{Instance: "initial", SessionName: "o/r-1"})
	if err != nil {
		t.Fatalf("FinalizeTask: %v", err)
	}
	if !result.Finalized || result.Definition != "local_okf" {
		t.Fatalf("result = %+v, want Finalized=true via local_okf", result)
	}
	data, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got := strings.TrimSpace(string(data)); got != "local-okf://kec/goals/x.md sha1" {
		t.Errorf("finalize action saw %q, want the resource id and revision", got)
	}
	st := store.Get("o/r-1").Tasks["initial"]
	if st == nil {
		t.Fatal("instance must not be reclaimed by finalize — cleanup is a separate step")
	}
	if st.FinalizedAt.IsZero() {
		t.Error("FinalizedAt must be recorded")
	}
}

// A resource whose finalize script fails must abort before cleanup — the
// instance is left in place for retry, rather than silently reclaimed with no
// completion record.
func TestFinalizeTask_ResourceFinalizeFailureAbortsBeforeCleanup(t *testing.T) {
	cfg := checkStatusOnlyConfig(t, 0)
	resourcesDir := filepath.Join(cfg.BaseDir, "resources")
	if err := os.MkdirAll(resourcesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourcesDir, "local-okf.toml"), []byte(`
[local_okf]
kind  = "resource_observer"
match = '^local-okf://'

[local_okf.observe]
type    = "exec"
command = "true"

[local_okf.finalize]
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 1"]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "work",
			Status:   contract.TaskStatusProduced,
			Resource: "local-okf://kec/goals/x.md",
			Observed: observedFacts(map[string]any{"checks_status": "SUCCESS"}),
		},
	})

	if _, err := FinalizeTask(cfg, store, FinalizeTaskParams{Instance: "initial", SessionName: "o/r-1"}); err == nil {
		t.Fatal("expected the finalize script failure to error")
	}
	if store.Get("o/r-1").Tasks["initial"] == nil {
		t.Error("instance must not be reclaimed when finalize fails")
	}
}

func TestFinalizeTask_InstanceRequired(t *testing.T) {
	cfg := checkStatusOnlyConfig(t, 0)
	store := testStore(t)
	if _, err := FinalizeTask(cfg, store, FinalizeTaskParams{SessionName: "o/r-1"}); err == nil {
		t.Fatal("expected an error for an empty instance")
	}
}
