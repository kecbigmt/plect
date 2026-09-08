package task

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// startSnapshotObserver records the order every node's OnStart fires in
// (across both the cleanup and the setup an invalidation triggers), plus a
// snapshot of watchID's TaskState at its own first OnStart — which lands
// between invalidateProducedNode stamping it failed and RunCleanup changing
// it further, the one point that state is otherwise unobservable from.
type startSnapshotObserver struct {
	tasks   map[string]*contract.TaskState
	watchID string
	starts  []string
	snapped bool
	status  string
	errMsg  string
}

func (o *startSnapshotObserver) OnStart(_, id string) {
	o.starts = append(o.starts, id)
	if id == o.watchID && !o.snapped {
		if st := o.tasks[id]; st != nil {
			o.status, o.errMsg = st.Status, st.Error
		}
		o.snapped = true
	}
}
func (o *startSnapshotObserver) OnSkip(string, string, string)                          {}
func (o *startSnapshotObserver) OnSuccess(string, string, time.Duration, []byte)        {}
func (o *startSnapshotObserver) OnFailure(string, string, time.Duration, error, []byte) {}

func TestRunSetup_ProducedRunScopedNodeAliveFails_RebuildsNodeAndDependents(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	aMarker := tmpDir + "/a-setup-ran"
	bMarker := tmpDir + "/b-setup-ran"
	plan := buildPlan(t,
		[]taskStub{
			{id: "a", scope: "run", setup: "touch " + aMarker + `; echo '{"value":"rebuilt"}'`, cleanup: "true", alive: "exit 1"},
			{id: "b", scope: "run", setup: "touch " + bMarker + "; echo '{}'", cleanup: "true"},
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
	obs := &startSnapshotObserver{tasks: tasks, watchID: "a"}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
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
	// b depends on a, so cleanup must release b before a (reverse dependency
	// order); setup then rebuilds forward, a before b.
	wantStarts := []string{"b", "a", "a", "b"}
	if !equalStrings(obs.starts, wantStarts) {
		t.Fatalf("start order = %v, want %v", obs.starts, wantStarts)
	}
	if obs.status != contract.TaskStatusFailed || obs.errMsg == "" {
		t.Fatalf("a's state when its cleanup began = status %q, error %q, want failed with the liveness error", obs.status, obs.errMsg)
	}
}

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

// releaseTrackingObserver implements both Observer and ReleaseObserver so a
// test can prove invalidateProducedNode flushes a same-pass release before
// the loop rebuilds the node it just released -- and that the loop honors a
// flush failure by aborting rather than proceeding to rebuild on an
// unpersisted release.
type releaseTrackingObserver struct {
	events   []string
	releases []map[string]*contract.TaskState
	failWith error
}

func (o *releaseTrackingObserver) OnStart(_, id string)          { o.events = append(o.events, "start:"+id) }
func (o *releaseTrackingObserver) OnSkip(string, string, string) {}
func (o *releaseTrackingObserver) OnSuccess(_, id string, _ time.Duration, _ []byte) {
	o.events = append(o.events, "success:"+id)
}
func (o *releaseTrackingObserver) OnFailure(_, id string, _ time.Duration, _ error, _ []byte) {
	o.events = append(o.events, "failure:"+id)
}
func (o *releaseTrackingObserver) OnRelease(released map[string]*contract.TaskState) error {
	o.events = append(o.events, "release")
	snapshot := make(map[string]*contract.TaskState, len(released))
	for k, v := range released {
		cp := *v
		snapshot[k] = &cp
	}
	o.releases = append(o.releases, snapshot)
	return o.failWith
}

// TestRunSetup_LivenessInvalidate_FlushesReleaseBeforeRebuild proves a
// same-pass liveness-invalidate-then-rebuild reports the release to a
// ReleaseObserver -- carrying the released row's own execution id -- before
// the rebuild's own setup begins, and marks the rebuilt state NewExecution
// so persistence never folds it into the row it just released.
func TestRunSetup_LivenessInvalidate_FlushesReleaseBeforeRebuild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	aMarker := tmpDir + "/a-setup-ran"
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "touch " + aMarker + `; echo '{"value":"rebuilt"}'`, cleanup: "true", alive: "exit 1"}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "stale"}, ExecutionID: "exec-1"},
	}
	obs := &releaseTrackingObserver{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("setup: %v", err)
	}

	wantEvents := []string{"start:a", "success:a", "release", "start:a", "success:a"}
	if !equalStrings(obs.events, wantEvents) {
		t.Fatalf("event order = %v, want %v (the release must be flushed before the rebuild starts)", obs.events, wantEvents)
	}
	if len(obs.releases) != 1 {
		t.Fatalf("OnRelease calls = %d, want 1", len(obs.releases))
	}
	released := obs.releases[0]["a"]
	if released == nil || released.Status != contract.TaskStatusCleaned || released.ExecutionID != "exec-1" {
		t.Fatalf("released state = %+v, want cleaned with the original execution id", released)
	}

	if tasks["a"].Status != contract.TaskStatusProduced || tasks["a"].Outputs["value"] != "rebuilt" {
		t.Fatalf("a = %+v, want rebuilt and produced", tasks["a"])
	}
	if tasks["a"].ExecutionID != "" {
		t.Fatalf("rebuilt a.ExecutionID = %q, want empty (a fresh generation, not a continuation of the released row)", tasks["a"].ExecutionID)
	}
	if !tasks["a"].NewExecution {
		t.Fatal("rebuilt a.NewExecution = false, want true: persistence must not silently adopt whatever row it finds")
	}
}

// TestRunSetup_LivenessInvalidate_ReleaseFlushFailureAbortsBeforeRebuild
// proves that when a ReleaseObserver cannot durably record the release, the
// rebuild never runs -- proceeding anyway would leave the in-memory state
// produced again with nothing on disk recording that the prior generation
// was ever released, exactly the collapse this checkpoint exists to avoid.
func TestRunSetup_LivenessInvalidate_ReleaseFlushFailureAbortsBeforeRebuild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmpDir := t.TempDir()
	aMarker := tmpDir + "/a-setup-ran"
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "touch " + aMarker + `; echo '{"value":"rebuilt"}'`, cleanup: "true", alive: "exit 1"}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{"value": "stale"}, ExecutionID: "exec-1"},
	}
	flushErr := errors.New("boom: disk full")
	obs := &releaseTrackingObserver{failWith: flushErr}

	err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs)
	if err == nil || !strings.Contains(err.Error(), flushErr.Error()) {
		t.Fatalf("RunSetup error = %v, want it to wrap %v", err, flushErr)
	}
	if _, statErr := exec.Command("bash", "-c", "test -f "+aMarker).CombinedOutput(); statErr == nil {
		t.Fatal("a's setup must not re-run when its release could not be flushed durably")
	}
	if tasks["a"].Status != contract.TaskStatusCleaned {
		t.Fatalf("a.Status = %q, want cleaned (the rebuild must not proceed on an unflushed release)", tasks["a"].Status)
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
