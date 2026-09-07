package service

import (
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// watchTaskSchema declares pr_state/checks_status as mutable, title immutable.
const watchTaskSchema = `
[outputs_schema]
type = "object"

[outputs_schema.properties.pr_state]
type = "string"
mutable = true

[outputs_schema.properties.checks_status]
type = "string"
mutable = true

[outputs_schema.properties.title]
type = "string"
`

func TestSetOutput_MergesMutableKeys(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "watch"}})

	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"watch": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"pr_state": "open", "title": "Old"},
			SetupAt: time.Now(),
		},
	})

	result, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Node:       "watch",
		Outputs:    map[string]any{"pr_state": "merged"},
	})
	if err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if result.Target != "watch" {
		t.Errorf("Target = %q, want %q", result.Target, "watch")
	}

	got := store.Get("org/repo-1").Nodes["watch"].Outputs
	if got["pr_state"] != "merged" {
		t.Errorf("pr_state = %v, want merged", got["pr_state"])
	}
	if got["title"] != "Old" {
		t.Errorf("merge must not touch absent keys; title = %v, want Old", got["title"])
	}
}

// SetOutput mutates an existing session's persisted outputs, so it must honor
// SessionGuard like the other per-session write paths.
func TestSetOutput_SessionGuardBlocksCrossOwner(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "watch"}})

	seedSessionWithNodes(t, store, "exampleorg/repo-26", "exampleorg/repo", 26, "wf", map[string]*contract.TaskState{
		"watch": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"pr_state": "open"},
			SetupAt: time.Now(),
		},
	})
	cfg.SessionGuard = "^acme/"

	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "exampleorg/repo-26",
		Node:       "watch",
		Outputs:    map[string]any{"pr_state": "merged"},
	})
	if err == nil {
		t.Fatal("expected session-guard rejection for cross-owner set-output")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRepoNotAllowed {
		t.Errorf("want ErrRepoNotAllowed, got %v", err)
	}
	if got := store.Get("exampleorg/repo-26").Nodes["watch"].Outputs["pr_state"]; got != "open" {
		t.Errorf("blocked set-output must not mutate the session; pr_state = %v", got)
	}
}

func TestSetOutput_RejectsImmutableKey(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "watch"}})
	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"watch": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{"title": "Old"}},
	})

	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Node:       "watch",
		Outputs:    map[string]any{"title": "New"},
	})
	if err == nil {
		t.Fatal("expected error for immutable key")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Errorf("error should mention immutability, got %q", err.Error())
	}
	if got := store.Get("org/repo-1").Nodes["watch"].Outputs["title"]; got != "Old" {
		t.Errorf("rejected write must not persist; title = %v", got)
	}
}

func TestSetOutput_RejectsWhenNoMutableDeclared(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "plain", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "plain"}})
	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"plain": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Node:       "plain",
		Outputs:    map[string]any{"anything": "x"},
	})
	if err == nil {
		t.Fatal("expected error: no schema means nothing is mutable (safe by default)")
	}
	if !strings.Contains(err.Error(), "no mutable output keys") {
		t.Errorf("unexpected message: %q", err.Error())
	}
}

func TestSetOutput_WorkdirAlwaysRejected(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "watch"}})
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", nil)

	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Node:       "watch",
		Outputs:    map[string]any{"workspace_dir": "/tmp/evil"},
	})
	if err == nil {
		t.Fatal("expected error: workdir is reserved")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("unexpected message: %q", err.Error())
	}
}

func TestSetOutput_RequiresProducedState(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "watch"}})
	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"watch": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusFailed},
	})

	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Node:       "watch",
		Outputs:    map[string]any{"pr_state": "merged"},
	})
	if err == nil {
		t.Fatal("expected error for non-produced task")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrNotProduced {
		t.Errorf("want ErrNotProduced, got %v", err)
	}
}

func TestSetOutput_MergedOutputsMustSatisfySchema(t *testing.T) {
	store := testStore(t)
	schema := `
[outputs_schema]
type = "object"

[outputs_schema.properties.pr_state]
type = "string"
enum = ["open", "merged", "closed"]
mutable = true
`
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'", extra: schema}},
		[]nodeFixture{{id: "watch"}})
	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"watch": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{"pr_state": "open"}},
	})

	_, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Node:       "watch",
		Outputs:    map[string]any{"pr_state": "bogus"},
	})
	if err == nil {
		t.Fatal("expected schema violation error")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("unexpected message: %q", err.Error())
	}
}

func TestSetOutput_ExactlyOneTargetRequired(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}

	for _, p := range []SetOutputParams{
		{Identifier: "x", Outputs: map[string]any{"a": 1}},                            // neither
		{Identifier: "x", Node: "n", Workflow: true, Outputs: map[string]any{"a": 1}}, // both
	} {
		if _, err := SetOutput(cfg, store, p); err == nil {
			t.Errorf("params %+v: expected target-selection error", p)
		}
	}
}

func TestSetOutput_WorkflowPseudoNode(t *testing.T) {
	store := testStore(t)
	workdirsRoot := t.TempDir()
	cfg := writeWorkflowFixture(t, workdirsRoot, "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})

	// The @workflow outputs contract lives on the workflow's provider.
	writeSetupWorkflow(t, cfg, "wf", providerEchoingOutputs("wf", `{"workdir":"/tmp/x"}`)+`
[wf.outputs_schema]
type = "object"

[wf.outputs_schema.properties.pr_state]
type = "string"
mutable = true

[wf.outputs_schema.properties.workdir]
type = "string"
`)

	// Simulate a Phase 3 session: workflow pseudo-node already produced.
	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd", "pr_state": "open"},
		},
	})

	result, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Workflow:   true,
		Outputs:    map[string]any{"pr_state": "merged"},
	})
	if err != nil {
		t.Fatalf("SetOutput --workflow: %v", err)
	}
	if result.Target != contract.WorkflowPseudoNodeID {
		t.Errorf("Target = %q, want %q", result.Target, contract.WorkflowPseudoNodeID)
	}
	got := store.Get("org/repo-1").Nodes[contract.WorkflowPseudoNodeID].Outputs
	if got["pr_state"] != "merged" {
		t.Errorf("pr_state = %v, want merged", got["pr_state"])
	}
	if got["workspace_dir"] != "/tmp/wd" {
		t.Errorf("workdir must be untouched, got %v", got["workspace_dir"])
	}
}

func TestSetOutput_Task_MergesMutableKeys(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "review", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "review"}})
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {
			Scope:    contract.TaskScopeSession,
			Status:   contract.TaskStatusProduced,
			TaskID:   "review",
			Resource: "pr-1",
			Outputs:  map[string]any{"pr_state": "open"},
		},
		"review#2": {
			Scope:    contract.TaskScopeSession,
			Status:   contract.TaskStatusProduced,
			TaskID:   "review",
			Resource: "pr-2",
			Outputs:  map[string]any{"checks_status": "FAILURE", "pr_state": "open"},
		},
	})

	result, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Task:       "review#1",
		Outputs:    map[string]any{"checks_status": "SUCCESS"},
	})
	if err != nil {
		t.Fatalf("SetOutput: %v", err)
	}
	if result.Target != "review#1" {
		t.Errorf("Target = %q, want review#1", result.Target)
	}
	got := store.Get("org/repo-1").Tasks["review#1"].Outputs
	if got["checks_status"] != "SUCCESS" {
		t.Errorf("checks_status = %v, want SUCCESS", got["checks_status"])
	}
	if got["pr_state"] != "open" {
		t.Errorf("merge must not touch absent keys; pr_state = %v", got["pr_state"])
	}
	other := store.Get("org/repo-1").Tasks["review#2"].Outputs
	if other["checks_status"] != "FAILURE" {
		t.Errorf("review#2 checks_status = %v, want unchanged FAILURE", other["checks_status"])
	}
}

func TestSetOutput_Task_RejectsImmutableKey(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "review", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "review"}})
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "review", Outputs: map[string]any{"title": "Old"}},
	})
	// title is immutable in watchTaskSchema.
	_, err := SetOutput(cfg, store, SetOutputParams{Identifier: "org/repo-1", Task: "review#1", Outputs: map[string]any{"title": "New"}})
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutable error, got %v", err)
	}
}

func TestSetOutput_Task_NotFound(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "review", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "review"}})
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{})
	_, err := SetOutput(cfg, store, SetOutputParams{Identifier: "org/repo-1", Task: "review#missing", Outputs: map[string]any{"checks_status": "SUCCESS"}})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestSetOutput_Task_RejectsStaticNode(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "watch", scope: "session", setup: "echo '{}'", extra: watchTaskSchema}},
		[]nodeFixture{{id: "watch"}})
	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"watch": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})
	_, err := SetOutput(cfg, store, SetOutputParams{Identifier: "org/repo-1", Task: "watch", Outputs: map[string]any{"checks_status": "SUCCESS"}})
	if err == nil || !strings.Contains(err.Error(), "static workflow node") {
		t.Fatalf("expected static-node rejection, got %v", err)
	}
}

func TestSetOutput_RejectsMultipleTargets(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	tests := []SetOutputParams{
		{Identifier: "org/repo-1", Node: "a", Task: "b", Outputs: map[string]any{"x": "y"}},
		{Identifier: "org/repo-1", Workflow: true, Task: "b", Outputs: map[string]any{"x": "y"}},
		{Identifier: "org/repo-1", Node: "a", Workflow: true, Outputs: map[string]any{"x": "y"}},
	}
	for _, tt := range tests {
		if _, err := SetOutput(cfg, store, tt); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("params %+v: expected exactly-one error, got %v", tt, err)
		}
	}
}

// nestedRuntimeInner is the inner task a nested fixture wraps: one mutable
// output for a routed write to land on.
const nestedRuntimeInner = `
[outputs_schema]
type = "object"

[outputs_schema.properties.pid]
type = "string"
mutable = true
`

// nestedRuntimeOuter re-exports that output under a different public name, so
// the test distinguishes routing from a plain merge into the stored map.
const nestedRuntimeOuter = `
inner = "runtime"

[outputs.bind]
agent_pid = { from = "inner.outputs.pid" }

[outputs_schema]
type = "object"

[outputs_schema.properties.agent_pid]
type = "string"
mutable = true
`

// TestSetOutput_NestedRoutesWriteToTheBoundInnerOutput covers a mutable write
// against a nested task: the public key is a projection, so the write lands
// on the inner output its direct binding publishes and the contract is
// re-read from there rather than merged over.
func TestSetOutput_NestedRoutesWriteToTheBoundInnerOutput(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{
			{id: "runtime", scope: "session", setup: "echo '{}'", extra: nestedRuntimeInner},
			{id: "team_runtime", scope: "session", extra: nestedRuntimeOuter},
		},
		[]nodeFixture{{id: "team_runtime"}})

	seedSessionWithNodes(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"team_runtime": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"agent_pid": "1"},
			Layers: []contract.LayerState{
				{EffectID: "team_runtime", Status: contract.TaskStatusProduced},
				{EffectID: "runtime", Status: contract.TaskStatusProduced, Outputs: map[string]any{"pid": "1"}},
			},
			SetupAt: time.Now(),
		},
	})

	if _, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Node:       "team_runtime",
		Outputs:    map[string]any{"agent_pid": "9"},
	}); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}

	st := store.Get("org/repo-1").Nodes["team_runtime"]
	if got := st.Layers[1].Outputs["pid"]; got != "9" {
		t.Errorf("inner pid = %v, want the routed write", got)
	}
	if got := st.Outputs["agent_pid"]; got != "9" {
		t.Errorf("public agent_pid = %v, want the re-read projection", got)
	}
}

// TestSetOutput_NestedDynamicInstanceRoutesThroughItsLayers covers the
// `--task` route: a dynamic instance of a nested task is the shape the
// motivating consumer has, and it must route a write the same way a static
// node does.
func TestSetOutput_NestedDynamicInstanceRoutesThroughItsLayers(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{
			{id: "runtime", scope: "session", setup: "echo '{}'", extra: nestedRuntimeInner},
			{id: "team_runtime", scope: "session", extra: nestedRuntimeOuter},
		},
		[]nodeFixture{{id: "runtime"}})

	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"team_runtime#1": {
			Scope:   contract.TaskScopeSession,
			TaskID:  "team_runtime",
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"agent_pid": "1"},
			Layers: []contract.LayerState{
				{EffectID: "team_runtime", Status: contract.TaskStatusProduced},
				{EffectID: "runtime", Status: contract.TaskStatusProduced, Outputs: map[string]any{"pid": "1"}},
			},
			SetupAt: time.Now(),
		},
	})

	if _, err := SetOutput(cfg, store, SetOutputParams{
		Identifier: "org/repo-1",
		Task:       "team_runtime#1",
		Outputs:    map[string]any{"agent_pid": "9"},
	}); err != nil {
		t.Fatalf("SetOutput: %v", err)
	}

	st := store.Get("org/repo-1").Tasks["team_runtime#1"]
	if got := st.Layers[1].Outputs["pid"]; got != "9" {
		t.Errorf("inner pid = %v, want the routed write", got)
	}
	if got := st.Outputs["agent_pid"]; got != "9" {
		t.Errorf("public agent_pid = %v, want the re-read projection", got)
	}
}
