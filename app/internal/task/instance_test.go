package task

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func TestInstanceKey(t *testing.T) {
	if got := InstanceKey("review", ""); got != "review" {
		t.Errorf("blank instance id: got %q", got)
	}
	if got := InstanceKey("review", "3"); got != "review#3" {
		t.Errorf("with instance id: got %q", got)
	}
}

func TestNextInstanceNumber(t *testing.T) {
	produced := func() *contract.TaskState {
		return &contract.TaskState{Status: contract.TaskStatusProduced}
	}

	t.Run("empty starts at 1", func(t *testing.T) {
		if got := NextInstanceNumber("review", map[string]*contract.TaskState{}); got != 1 {
			t.Errorf("got %d, want 1", got)
		}
	})

	t.Run("increments per task, independently", func(t *testing.T) {
		tasks := map[string]*contract.TaskState{
			"review#1": produced(),
			"review#2": produced(),
			"build#1":  produced(),
		}
		if got := NextInstanceNumber("review", tasks); got != 3 {
			t.Errorf("review: got %d, want 3", got)
		}
		if got := NextInstanceNumber("build", tasks); got != 2 {
			t.Errorf("build: got %d, want 2 (independent numbering)", got)
		}
	})

	t.Run("monotonic across cleaned (no reuse)", func(t *testing.T) {
		tasks := map[string]*contract.TaskState{
			"review#1": {Status: contract.TaskStatusCleaned},
			"review#2": {Status: contract.TaskStatusCleaned},
		}
		if got := NextInstanceNumber("review", tasks); got != 3 {
			t.Errorf("got %d, want 3 (cleaned instances still occupy numbers)", got)
		}
	})

	t.Run("ignores non-numeric and prefix-only keys", func(t *testing.T) {
		tasks := map[string]*contract.TaskState{
			"review":     produced(), // static node, no suffix
			"review#abc": produced(), // non-numeric suffix
			"reviewer#9": produced(), // different task (no '#': "reviewer" != prefix "review#")
			"review#7":   produced(),
			"@workflow":  produced(),
		}
		if got := NextInstanceNumber("review", tasks); got != 8 {
			t.Errorf("got %d, want 8", got)
		}
	})
}

// resolveDef compiles a single definition the way the dynamic path does.
func resolveDef(t *testing.T, def config.TaskDefinition, key string) Resolved {
	t.Helper()
	r, err := ResolveDefinition(def, key)
	if err != nil {
		t.Fatalf("ResolveDefinition: %v", err)
	}
	return r
}

func TestExecuteTaskSetup_ParsesOutputs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	def := config.TaskDefinition{ID: "review", Scope: "session", Setup: shellStub(`echo '{"ready":"yes"}'`)}
	r := resolveDef(t, def, "review#1")

	result, err := ExecuteTaskSetup(context.Background(), r, nil, SessionVars{Name: "x"}, map[string]*contract.TaskState{})
	if err != nil {
		t.Fatalf("ExecuteTaskSetup: %v", err)
	}
	if result.Outputs["ready"] != "yes" {
		t.Errorf("outputs = %v", result.Outputs)
	}
}

func TestExecuteTaskSetup_InputSchemaRejection(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	def := config.TaskDefinition{
		ID:    "work",
		Scope: "session",
		Setup: shellStub(`echo '{}'`),
		InputsSchema: map[string]any{
			"type":     "object",
			"required": []any{"intent"},
			"properties": map[string]any{
				"intent": map[string]any{"type": "string"},
			},
		},
	}
	r := resolveDef(t, def, "work#1")
	// No intent bound → schema validation fails; ExecuteTaskSetup returns an
	// error and writes no state (the caller persists produced/failed).
	if _, err := ExecuteTaskSetup(context.Background(), r, map[string]any{}, SessionVars{}, nil); err == nil {
		t.Fatal("expected input schema validation error")
	}
}

// A dynamic instance's cleanup releases what that instance produced, so it
// reads the outputs persisted for it rather than anything the session as a
// whole carries. The cleanup surface observes no resource root at all: an
// effect that has to release something resource-shaped records it as an
// output at setup, where the resource is observable.
func TestRunCleanup_DynamicInstanceReadsItsOwnOutputs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	marker := filepath.Join(t.TempDir(), "resource")
	r := Resolved{NodeID: "review#1", Scope: "session", Cleanup: &lang.Action{
		Type:   lang.ActionShell,
		Script: `echo "$resource" > "$marker"`,
		Bind: map[string]*lang.Value{
			"resource": {Form: lang.FormFrom, From: "self.outputs.resource"},
			"marker":   {Form: lang.FormLiteral, Literal: marker},
		},
	}}
	tasks := map[string]*contract.TaskState{
		"review#1": {Scope: "session", Status: contract.TaskStatusProduced, Resource: "pr-99", Outputs: map[string]any{"resource": "pr-99"}},
	}
	if err := RunCleanup(context.Background(), []Resolved{r}, SessionVars{ResourceID: "session-res"}, tasks, nil); err != nil {
		t.Fatalf("RunCleanup: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "pr-99" {
		t.Errorf("cleanup self.outputs.resource = %q, want pr-99", got)
	}
}

func TestExecuteTaskSetup_SeesWorkflowOutputs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	def := config.TaskDefinition{ID: "review", Scope: "session", Setup: &lang.Action{
		Type:   lang.ActionShell,
		Script: `printf '{"branch":"%s"}' "$branch"`,
		Bind:   map[string]*lang.Value{"branch": {Form: lang.FormFrom, From: "workflow.outputs.branch"}},
	}}
	r := resolveDef(t, def, "review#1")
	wfTasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {Status: contract.TaskStatusProduced, Outputs: map[string]any{"branch": "feat/x"}},
	}
	result, err := ExecuteTaskSetup(context.Background(), r, nil, SessionVars{}, wfTasks)
	if err != nil {
		t.Fatalf("ExecuteTaskSetup: %v", err)
	}
	if result.Outputs["branch"] != "feat/x" {
		t.Errorf("expected @workflow outputs visible, got %v", result.Outputs)
	}
}

// TestExecuteTaskSetup_NestedRunsTheWholeChain covers the dynamic
// instantiation path: `plect task setup` on a nested task runs every layer,
// so the two setup entry points agree on what instantiating one task means.
func TestExecuteTaskSetup_NestedRunsTheWholeChain(t *testing.T) {
	exec := withScriptedExecutor(t, &scriptedExecutor{stdout: map[string]string{
		"outer-setup": `{"guard_dir":"/tmp/guard"}`,
		"inner-setup": `{"pid":"7"}`,
	}})
	ordered := nestedPlan(t,
		config.TaskDefinition{ID: "outer", Scope: "run", Setup: shellStub("outer-setup")},
		config.TaskDefinition{ID: "inner", Scope: "run", Setup: shellStub("inner-setup")},
	)
	result, err := ExecuteTaskSetup(context.Background(), ordered[0], nil, SessionVars{Name: "s"}, nil)
	if err != nil {
		t.Fatalf("ExecuteTaskSetup: %v", err)
	}
	if got := exec.commands(); !reflect.DeepEqual(got, []string{"outer-setup", "inner-setup"}) {
		t.Fatalf("setup order = %v, want the whole chain outside-in", got)
	}
	if len(result.Layers) != 2 || result.Layers[1].Outputs["pid"] != "7" {
		t.Errorf("Layers = %+v, want a record per layer with the innermost's outputs", result.Layers)
	}
}

// TestExecuteTaskSetup_NestedFailureKeepsProducedLayers covers the
// partial-failure contract of the dynamic path: the caller persists what
// produced so the next cleanup can unwind it.
func TestExecuteTaskSetup_NestedFailureKeepsProducedLayers(t *testing.T) {
	withScriptedExecutor(t, &scriptedExecutor{
		stdout: map[string]string{"outer-setup": `{"guard_dir":"/tmp/guard"}`},
		failOn: "inner-setup",
	})
	ordered := nestedPlan(t,
		config.TaskDefinition{ID: "outer", Scope: "run", Setup: shellStub("outer-setup")},
		config.TaskDefinition{ID: "inner", Scope: "run", Setup: shellStub("inner-setup")},
	)
	result, err := ExecuteTaskSetup(context.Background(), ordered[0], nil, SessionVars{Name: "s"}, nil)
	if err == nil {
		t.Fatal("ExecuteTaskSetup: want an error when the inner setup fails, got nil")
	}
	if len(result.Layers) != 2 {
		t.Fatalf("Layers = %+v, want a record for the produced layer and the failed one", result.Layers)
	}
	if result.Layers[0].Status != contract.TaskStatusProduced {
		t.Errorf("outer layer status = %q, want %q", result.Layers[0].Status, contract.TaskStatusProduced)
	}
	if result.Layers[1].Status != contract.TaskStatusFailed {
		t.Errorf("inner layer status = %q, want %q", result.Layers[1].Status, contract.TaskStatusFailed)
	}
}
