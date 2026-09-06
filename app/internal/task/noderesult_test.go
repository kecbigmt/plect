package task

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// resultCall captures one ResultObserver.OnResult invocation for assertions.
type resultCall struct {
	scope, node, effect, action, result string
	elapsed                             time.Duration
	body                                string
}

// resultRecordingObserver implements both Observer (so RunSetup/RunCleanup
// accept it) and ResultObserver (so it also captures every plect.node.result
// report), the same composition app/internal/service's nodeResultObserver
// performs against whatever CLI/UI Observer a caller supplies.
type resultRecordingObserver struct {
	results []resultCall
}

func (r *resultRecordingObserver) OnStart(string, string)                                 {}
func (r *resultRecordingObserver) OnSkip(string, string, string)                          {}
func (r *resultRecordingObserver) OnSuccess(string, string, time.Duration, []byte)        {}
func (r *resultRecordingObserver) OnFailure(string, string, time.Duration, error, []byte) {}

func (r *resultRecordingObserver) OnResult(scope, node, effectID, action, result string, elapsed time.Duration, body string) {
	r.results = append(r.results, resultCall{scope: scope, node: node, effect: effectID, action: action, result: result, elapsed: elapsed, body: body})
}

func TestRunSetup_ReportsNodeResult_Produced(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "agent", scope: "run", setup: `echo '{}'`}},
		[]nodeStub{{id: "agent"}},
	)
	tasks := map[string]*contract.TaskState{}
	obs := &resultRecordingObserver{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if len(obs.results) != 1 {
		t.Fatalf("results = %v, want exactly one", obs.results)
	}
	got := obs.results[0]
	if got.scope != "run" || got.node != "agent" || got.effect != "agent" {
		t.Fatalf("scope/node/effect = %q/%q/%q", got.scope, got.node, got.effect)
	}
	if got.action != event.NodeResultActionSetup || got.result != event.NodeResultProduced {
		t.Fatalf("action/result = %q/%q, want setup/produced", got.action, got.result)
	}
	if got.body != "" {
		t.Fatalf("body = %q, want empty on success", got.body)
	}
}

func TestRunSetup_ReportsNodeResult_Failed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "agent", scope: "run", setup: `echo boom 1>&2; exit 1`}},
		[]nodeStub{{id: "agent"}},
	)
	tasks := map[string]*contract.TaskState{}
	obs := &resultRecordingObserver{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err == nil {
		t.Fatal("expected error")
	}
	if len(obs.results) != 1 {
		t.Fatalf("results = %v, want exactly one", obs.results)
	}
	got := obs.results[0]
	if got.action != event.NodeResultActionSetup || got.result != event.NodeResultFailed {
		t.Fatalf("action/result = %q/%q, want setup/failed", got.action, got.result)
	}
	if got.body != "boom" {
		t.Fatalf("body = %q, want the captured stderr tail", got.body)
	}
}

func TestRunSetup_ReportsNodeResult_AliveSkip(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "agent", scope: "run", setup: `echo '{}'`, alive: "true"}},
		[]nodeStub{{id: "agent"}},
	)
	tasks := map[string]*contract.TaskState{
		"agent": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	obs := &resultRecordingObserver{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if len(obs.results) != 1 {
		t.Fatalf("results = %v, want exactly one", obs.results)
	}
	got := obs.results[0]
	if got.action != event.NodeResultActionAlive || got.result != event.NodeResultSkipped {
		t.Fatalf("action/result = %q/%q, want alive/skipped", got.action, got.result)
	}
}

// TestRunSetup_ReportsNodeResult_AliveFailureThenRebuild pins the failing
// half of the liveness check: it is a completed action in its own right
// (action=alive, result=failed), reported before the cleanup and re-setup
// it then triggers — not folded into either of theirs, since the rebuild
// that follows can succeed even though the check itself did not.
func TestRunSetup_ReportsNodeResult_AliveFailureThenRebuild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "agent", scope: "run", setup: `echo '{}'`, cleanup: "true", alive: "echo gone 1>&2; exit 1"}},
		[]nodeStub{{id: "agent"}},
	)
	tasks := map[string]*contract.TaskState{
		"agent": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	obs := &resultRecordingObserver{}
	if err := RunSetup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if len(obs.results) != 3 {
		t.Fatalf("results = %+v, want alive/failed, cleanup/cleaned, setup/produced", obs.results)
	}
	if got := obs.results[0]; got.action != event.NodeResultActionAlive || got.result != event.NodeResultFailed {
		t.Fatalf("first result = %+v, want alive/failed", got)
	}
	if body := obs.results[0].body; !strings.Contains(body, "gone") {
		t.Fatalf("alive failure body = %q, want it to carry the probe's stderr", body)
	}
	if got := obs.results[1]; got.action != event.NodeResultActionCleanup || got.result != event.NodeResultCleaned {
		t.Fatalf("second result = %+v, want cleanup/cleaned", got)
	}
	if got := obs.results[2]; got.action != event.NodeResultActionSetup || got.result != event.NodeResultProduced {
		t.Fatalf("third result = %+v, want setup/produced", got)
	}
}

// TestResolveWorkflowNodes_EffectIsCatalogQualified pins that a
// plect.node.result's effect identifies the node's `uses` target — the
// address a definitions map (as app/internal/config.LoadTaskDefinitions
// returns it) is actually keyed by — not the definition's own local
// TaskDefinition.ID, which differs whenever the definition is plugin-owned.
func TestResolveWorkflowNodes_EffectIsCatalogQualified(t *testing.T) {
	defs := map[string]config.TaskDefinition{
		"official.example.agent": {ID: "agent", Scope: "run", Setup: shellStub("echo '{}'")},
	}
	wf := config.WorkflowFile{ID: "wf", Nodes: []config.WorkflowNode{
		{ID: "worker", Uses: "official.example.agent"},
	}}
	plan, err := CompileWorkflow(wf, defs)
	if err != nil {
		t.Fatalf("CompileWorkflow: %v", err)
	}
	if len(plan.Run) != 1 {
		t.Fatalf("plan.Run = %+v, want one node", plan.Run)
	}
	if got := plan.Run[0].TaskID; got != "official.example.agent" {
		t.Fatalf("TaskID = %q, want the catalog-qualified uses target", got)
	}
}

func TestRunCleanup_ReportsNodeResult_Cleaned(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "agent", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeStub{{id: "agent"}},
	)
	tasks := map[string]*contract.TaskState{
		"agent": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	obs := &resultRecordingObserver{}
	if err := RunCleanup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(obs.results) != 1 {
		t.Fatalf("results = %v, want exactly one", obs.results)
	}
	got := obs.results[0]
	if got.action != event.NodeResultActionCleanup || got.result != event.NodeResultCleaned {
		t.Fatalf("action/result = %q/%q, want cleanup/cleaned", got.action, got.result)
	}
}

func TestRunCleanup_ReportsNodeResult_Failed(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "agent", scope: "run", setup: `echo '{}'`, cleanup: "echo boom 1>&2; exit 1"}},
		[]nodeStub{{id: "agent"}},
	)
	tasks := map[string]*contract.TaskState{
		"agent": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	obs := &resultRecordingObserver{}
	if err := RunCleanup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err == nil {
		t.Fatal("expected error")
	}
	if len(obs.results) != 1 {
		t.Fatalf("results = %v, want exactly one", obs.results)
	}
	got := obs.results[0]
	if got.action != event.NodeResultActionCleanup || got.result != event.NodeResultFailed {
		t.Fatalf("action/result = %q/%q, want cleanup/failed", got.action, got.result)
	}
	if got.body != "boom" {
		t.Fatalf("body = %q, want the captured stderr tail", got.body)
	}
}

func TestRunCleanup_SkipReportsNoNodeResult(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	plan := buildPlan(t,
		[]taskStub{{id: "agent", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeStub{{id: "agent"}},
	)
	// No entry in tasks for "agent": RunCleanup takes its "no setup state"
	// skip branch, which did no work and so must report nothing.
	tasks := map[string]*contract.TaskState{}
	obs := &resultRecordingObserver{}
	if err := RunCleanup(context.Background(), plan.Run, SessionVars{}, tasks, obs); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(obs.results) != 0 {
		t.Fatalf("results = %v, want none for a no-op skip", obs.results)
	}
}
