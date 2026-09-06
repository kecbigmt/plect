package task

import (
	"context"
	"os/exec"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// Given a produced run-scoped node whose alive fails, when plect up runs,
// then the node is cleaned, marked failed with the liveness error, its
// produced dependents are cleaned in reverse order, and setup re-runs from
// that node; the result reads up with every current-plan node produced.
func TestRunSetup_ProducedRunScopedNodeAliveFails_RebuildsNodeAndDependents(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	aMarker := tmpDir + "/a-setup-ran"
	bMarker := tmpDir + "/b-setup-ran"
	plan := buildPlan(t,
		[]taskStub{
			{id: "a", scope: "run", setup: "touch " + aMarker + `; echo '{"value":"rebuilt"}'`, alive: "exit 1"},
			{id: "b", scope: "run", setup: "touch " + bMarker + "; echo '{}'"},
		},
		[]nodeStub{
			{id: "a"},
			{id: "b", inputs: map[string]*lang.Value{"a_dep": fromValue("nodes.a.outputs.value")}},
		},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "stale"}},
		"b": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+aMarker).CombinedOutput(); statErr != nil {
		t.Fatal("a's setup did not re-run after its liveness probe failed")
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+bMarker).CombinedOutput(); statErr != nil {
		t.Fatal("b, a produced dependent of the invalidated node, was not rebuilt")
	}
	if tasks["a"].Status != contract.TaskStatusProduced || tasks["a"].Outputs["value"] != "rebuilt" {
		t.Fatalf("a = %+v, want rebuilt and produced", tasks["a"])
	}
	if tasks["b"].Status != contract.TaskStatusProduced {
		t.Fatalf("b.Status = %q, want produced", tasks["b"].Status)
	}
}

// Given a produced session-scoped node whose alive fails, when plect up
// runs, then it is rebuilt the same way, and its dependents (run-scoped
// nodes reading its outputs) are rebuilt after it.
func TestRunSetup_ProducedSessionScopedNodeAliveFails_RebuildsRunScopedDependent(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{
			{id: "guard", scope: "session", setup: `echo '{"k":"the-dir"}'`, alive: "exit 1"},
			{id: "agent", scope: "run", setupAction: &lang.Action{
				Type:   lang.ActionShell,
				Script: `jq -nc --arg saw "$saw" '{saw:$saw}'`,
				Bind:   map[string]*lang.Value{"saw": {Form: lang.FormFrom, From: "nodes.guard.outputs.k"}},
			}},
		},
		[]nodeStub{
			{id: "guard"},
			{id: "agent", inputs: map[string]*lang.Value{"link": fromValue("nodes.guard.outputs.k")}},
		},
	)
	tasks := map[string]*contract.TaskState{
		"guard": {Scope: "session", Status: contract.TaskStatusProduced, Outputs: map[string]any{"k": "stale-dir"}},
		"agent": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"saw": "stale-dir"}},
	}
	if err := RunSetup(context.Background(), plan.UpOrder(), SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if tasks["guard"].Outputs["k"] != "the-dir" {
		t.Fatalf("guard was not rebuilt: %+v", tasks["guard"])
	}
	if tasks["agent"].Outputs["saw"] != "the-dir" {
		t.Fatalf("agent, a run-scoped dependent, was not rebuilt after guard: %+v", tasks["agent"])
	}
	if tasks["guard"].Status != contract.TaskStatusProduced || tasks["agent"].Status != contract.TaskStatusProduced {
		t.Fatalf("guard = %q, agent = %q, want both produced", tasks["guard"].Status, tasks["agent"].Status)
	}
}

// Given a produced node whose alive is noop, when plect up runs, then no
// process is started for it and it is skipped.
func TestRunSetup_NoopAliveSkipsWithoutRunningProcess(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	marker := tmpDir + "/setup-ran"
	def := config.TaskDefinition{
		ID: "a", Scope: "run",
		Setup:  shellStub("touch " + marker + "; echo '{}'"),
		Health: &config.HealthConfig{Alive: &lang.Action{Type: lang.ActionNoop}},
	}
	ordered := nestedPlan(t, def)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "preserved"}},
	}
	obs := &recordingObserver{}
	if err := RunSetup(context.Background(), ordered, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+marker).CombinedOutput(); statErr == nil {
		t.Fatal("a noop alive probe must not run the setup process")
	}
	if len(obs.skips) != 1 || obs.skips[0] != "a" {
		t.Fatalf("skips = %v, want [a]", obs.skips)
	}
	if tasks["a"].Outputs["value"] != "preserved" {
		t.Fatalf("expected preserved outputs, got %v", tasks["a"].Outputs)
	}
}

// Given a produced node whose alive succeeds, when plect up runs, then it is
// skipped and its record is untouched.
func TestRunSetup_PassingAliveSkipsAndRecordUntouched(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	marker := tmpDir + "/setup-ran"
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "touch " + marker + "; echo '{}'", alive: "true"}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "preserved"}},
	}
	obs := &recordingObserver{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+marker).CombinedOutput(); statErr == nil {
		t.Fatal("setup ran even though the alive probe passed")
	}
	if len(obs.skips) != 1 || obs.skips[0] != "a" {
		t.Fatalf("skips = %v, want [a]", obs.skips)
	}
	if tasks["a"].Outputs["value"] != "preserved" {
		t.Fatalf("expected preserved outputs, got %v", tasks["a"].Outputs)
	}
}

// Given a session that reads up with every probe passing, when plect up
// runs, then no node is rebuilt and the command exits zero.
func TestRunSetup_EveryProbePassing_NoRebuild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	sessionMarker := tmpDir + "/session-setup-ran"
	runMarker := tmpDir + "/run-setup-ran"
	plan := buildPlan(t,
		[]taskStub{
			{id: "guard", scope: "session", setup: "touch " + sessionMarker + `; echo '{"k":"the-dir"}'`, alive: "true"},
			{id: "runner", scope: "run", setup: "touch " + runMarker + "; echo '{}'", alive: "true"},
		},
		[]nodeStub{{id: "guard"}, {id: "runner"}},
	)
	tasks := map[string]*contract.TaskState{
		"guard":  {Scope: "session", Status: contract.TaskStatusProduced, Outputs: map[string]any{"k": "the-dir"}},
		"runner": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	if err := RunSetup(context.Background(), plan.UpOrder(), SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, marker := range []string{sessionMarker, runMarker} {
		if _, statErr := exec.Command("bash", "-c", "test -f "+marker).CombinedOutput(); statErr == nil {
			t.Fatalf("setup ran for %s even though every probe passed", marker)
		}
	}
}

// Given a probe that cannot resolve self.outputs.<key>, when plect up runs,
// then the node is treated as invalid, not as skipped.
func TestRunSetup_UnresolvedSelfOutputInProbe_InvalidatesRatherThanSkips(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	marker := tmpDir + "/setup-ran"
	def := config.TaskDefinition{
		ID: "a", Scope: "run",
		Setup: shellStub("touch " + marker + "; echo '{}'"),
		Health: &config.HealthConfig{Alive: &lang.Action{
			Type:   lang.ActionShell,
			Script: "true",
			Bind:   map[string]*lang.Value{"missing": fromValue("self.outputs.absent_key")},
		}},
	}
	ordered := nestedPlan(t, def)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	if err := RunSetup(context.Background(), ordered, SessionVars{}, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+marker).CombinedOutput(); statErr != nil {
		t.Fatal("a probe that cannot resolve must invalidate the node rather than let it be skipped")
	}
	if tasks["a"].Status != contract.TaskStatusProduced {
		t.Fatalf("a.Status = %q, want produced after rebuild", tasks["a"].Status)
	}
}

// Given a nesting chain whose inner layer's alive fails, when plect up runs,
// then the whole node is cleaned layer by layer (LIFO) and rebuilt.
func TestRunSetup_NestedInnerLayerAliveFails_RebuildsWholeChainLIFO(t *testing.T) {
	executor := withScriptedExecutor(t, &scriptedExecutor{})
	inner := config.TaskDefinition{
		ID: "inner", Scope: "run",
		Setup:   shellStub("inner-setup"),
		Cleanup: shellStub("inner-cleanup"),
		Health:  &config.HealthConfig{Alive: shellStub("inner-alive")},
	}
	outer := config.TaskDefinition{
		ID: "outer", Scope: "run",
		Setup:   shellStub("outer-setup"),
		Cleanup: shellStub("outer-cleanup"),
	}
	ordered := nestedPlan(t, outer, inner)
	tasks := map[string]*contract.TaskState{}
	if err := RunSetup(context.Background(), ordered, SessionVars{Name: "s"}, tasks, nil); err != nil {
		t.Fatalf("initial setup: %v", err)
	}
	if tasks["outer"].Status != contract.TaskStatusProduced {
		t.Fatalf("initial outer.Status = %q, want produced", tasks["outer"].Status)
	}

	executor.failOn = "inner-alive"
	executor.reset()
	if err := RunSetup(context.Background(), ordered, SessionVars{Name: "s"}, tasks, nil); err != nil {
		t.Fatalf("re-run after inner liveness failure: %v", err)
	}
	want := []string{"inner-alive", "inner-cleanup", "outer-cleanup", "outer-setup", "inner-setup"}
	if got := executor.commands(); !equalStrings(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	if tasks["outer"].Status != contract.TaskStatusProduced {
		t.Fatalf("rebuilt outer.Status = %q, want produced", tasks["outer"].Status)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Failure path: cleanup of a produced dependent fails midway. The walk
// records the failed node and stops, exactly as an ordinary cleanup failure
// does, and the invalidated node's own setup never runs.
func TestRunSetup_DependentCleanupFailureStopsTheWalk(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	aMarker := tmpDir + "/a-setup-ran"
	plan := buildPlan(t,
		[]taskStub{
			{id: "a", scope: "run", setup: "touch " + aMarker + "; echo '{\"value\":\"rebuilt\"}'", alive: "exit 1"},
			{id: "b", scope: "run", setup: "echo '{}'", cleanup: "exit 1"},
		},
		[]nodeStub{
			{id: "a"},
			{id: "b", inputs: map[string]*lang.Value{"a_dep": fromValue("nodes.a.outputs.value")}},
		},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "stale"}},
		"b": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("RunSetup: want an error when a dependent's cleanup fails, got nil")
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+aMarker).CombinedOutput(); statErr == nil {
		t.Fatal("a's setup must not run when a dependent's cleanup failed")
	}
	if tasks["b"].Status != contract.TaskStatusFailed {
		t.Fatalf("b.Status = %q, want failed", tasks["b"].Status)
	}
}

// Failure path: after a produced node is invalidated and cleaned, its
// rebuild's own setup fails. RunSetup reports that failure exactly like an
// ordinary setup failure.
func TestRunSetup_RebuiltNodeSetupFailure(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "exit 1", alive: "exit 1"}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "stale"}},
	}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, nil); err == nil {
		t.Fatal("RunSetup: want an error when the rebuilt node's own setup fails, got nil")
	}
	if tasks["a"].Status != contract.TaskStatusFailed {
		t.Fatalf("a.Status = %q, want failed", tasks["a"].Status)
	}
}
