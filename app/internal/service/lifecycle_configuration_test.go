package service

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

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

	first, err := lifecycleConfigurationDigest(plan, nil, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest: %v", err)
	}
	second, err := lifecycleConfigurationDigest(plan, nil, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest: %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("digest = %q, %q, want two equal non-empty digests", first, second)
	}
}

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
	beforeDigest, err := lifecycleConfigurationDigest(beforePlan, nil, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	afterPlan, err := buildPlanForSession(after, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession (after): %v", err)
	}
	afterDigest, err := lifecycleConfigurationDigest(afterPlan, nil, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (after): %v", err)
	}

	if beforeDigest == afterDigest {
		t.Fatalf("digest unchanged (%q) after the setup action changed", beforeDigest)
	}
}

// The desired workflow can reuse a node id for a different declaration
// than the one an outstanding execution under that same id still retains
// (an in-flight `uses` edit). Both must be projected: the desired one by
// the ordinary plan walk, and the retained one by its own identity,
// keyed apart so neither overwrites the other.
func TestLifecycleConfigurationDigest_RetainsOutstandingCleanupOnNodeIDReuse(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "old_def", scope: "run", cleanup: "true"},
			{id: "new_def", scope: "run", setup: `echo '{}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "shared", uses: "new_def"}},
	)
	session := &domain.Session{
		Workflow: "coding",
		Nodes: map[string]*contract.TaskState{
			"shared": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "old_def", Seq: 1, Outputs: map[string]any{}},
		},
	}
	plan, err := buildPlanForSession(cfg, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}

	outstanding, err := unifiedTeardownList(cfg, session, false)
	if err != nil {
		t.Fatalf("unifiedTeardownList: %v", err)
	}
	before, err := lifecycleConfigurationDigest(plan, outstanding, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	rewriteTaskFixture(t, cfg, taskFixture{id: "old_def", scope: "run", cleanup: "echo changed"})

	outstandingAfter, err := unifiedTeardownList(cfg, session, false)
	if err != nil {
		t.Fatalf("unifiedTeardownList (after): %v", err)
	}
	after, err := lifecycleConfigurationDigest(plan, outstandingAfter, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (after): %v", err)
	}

	if before == after {
		t.Fatalf("digest unchanged (%q) after the node-id-colliding outstanding execution's retained cleanup changed", before)
	}
}

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

// The baseline is one session-wide value shared by up/down/destroy: an
// ordinary Up's own execution never touches a dynamic task instance, but
// the digest it records must still cover one, or a later Down repairing
// that instance's cleanup would find nothing changed.
func TestUp_BaselineAccountsForDynamicOutstandingExecution(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"},
			{id: "adhoc", scope: "run", cleanup: "true"},
		},
		[]nodeFixture{{id: "build"}},
	)
	store := testStore(t)
	seedSessionSplit(t, store, "sess-1", "acme", 1, "coding",
		map[string]*contract.TaskState{},
		map[string]*contract.TaskState{
			"adhoc#1": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "adhoc", Seq: 1, Outputs: map[string]any{}},
		},
	)

	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Up (first): %v", err)
	}
	baseline := store.Get("sess-1").LifecycleConfigurationDigest

	rewriteTaskFixture(t, cfg, taskFixture{id: "adhoc", scope: "run", cleanup: "echo changed"})

	changed, err := Up(cfg, store, UpParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Up (second): %v", err)
	}
	if changed.LifecycleConfigurationWarning == "" {
		t.Fatal("Up: want a warning after the dynamic instance's cleanup changed")
	}
	if store.Get("sess-1").LifecycleConfigurationDigest == baseline {
		t.Fatal("baseline did not advance")
	}
}

// The cross-operation risk a session-wide digest exists to avoid: if Up's
// own (node-only) executable scope leaked into what it compares, an
// outstanding dynamic instance untouched by that Up would make Down's
// (session-wide) comparison look like a change when nothing changed at all.
func TestUp_ThenDown_UnchangedConfigurationWithDynamicOutstandingExecutionDoesNotWarn(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"},
			{id: "adhoc", scope: "run", cleanup: "true"},
		},
		[]nodeFixture{{id: "build"}},
	)
	store := testStore(t)
	seedSessionSplit(t, store, "sess-1", "acme", 1, "coding",
		map[string]*contract.TaskState{},
		map[string]*contract.TaskState{
			"adhoc#1": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "adhoc", Seq: 1, Outputs: map[string]any{}},
		},
	)

	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	result, err := Down(cfg, store, DownParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if result.LifecycleConfigurationWarning != "" {
		t.Errorf("LifecycleConfigurationWarning = %q, want no warning: nothing changed between the Up that recorded the baseline and this Down", result.LifecycleConfigurationWarning)
	}
}

// Down only ever tears down run-scoped nodes, but the digest it records
// must still cover an outstanding session-scoped one, or a later operation
// repairing that node's cleanup would find nothing changed.
func TestDown_BaselineAccountsForSessionScopedOutstandingExecution(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"},
			{id: "review", scope: "session", cleanup: "true"},
		},
		[]nodeFixture{{id: "build"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{
		"review": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "review", Seq: 1, Outputs: map[string]any{}},
	})

	if _, err := Down(cfg, store, DownParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Down (first): %v", err)
	}
	baseline := store.Get("sess-1").LifecycleConfigurationDigest

	rewriteTaskFixture(t, cfg, taskFixture{id: "review", scope: "session", cleanup: "echo changed"})

	changed, err := Down(cfg, store, DownParams{Identifier: "sess-1"})
	if err != nil {
		t.Fatalf("Down (second): %v", err)
	}
	if changed.LifecycleConfigurationWarning == "" {
		t.Fatal("Down: want a warning after the session-scoped node's cleanup changed")
	}
	if store.Get("sess-1").LifecycleConfigurationDigest == baseline {
		t.Fatal("baseline did not advance")
	}
}

// The baseline records which configuration this attempt used; a failure
// in the attempt itself (as opposed to a precondition failure before it
// starts) does not make that fact untrue.
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

// A literal's Go type is part of its identity: TOML parses an integer and a
// float literal into int64 and float64 respectively, and fmt's %v renders
// both as "1", so projectValue must not go through that formatting.
func TestProjectValue_LiteralsOfDifferentTypesDoNotCollide(t *testing.T) {
	intJSON, err := json.Marshal(projectValue(&lang.Value{Form: lang.FormLiteral, Literal: int64(1)}))
	if err != nil {
		t.Fatalf("marshal int projection: %v", err)
	}
	floatJSON, err := json.Marshal(projectValue(&lang.Value{Form: lang.FormLiteral, Literal: float64(1)}))
	if err != nil {
		t.Fatalf("marshal float projection: %v", err)
	}
	if string(intJSON) == string(floatJSON) {
		t.Fatalf("int64(1) and float64(1) projected identically: %s", intJSON)
	}
}

// The workspace provider's own setup/cleanup runs once per session via the
// @workflow pseudo-node, outside plan.UpOrder()'s ordinary nodes, so the
// digest must resolve it separately.
func TestLifecycleConfigurationDigest_ChangesWhenWorkspaceProviderCleanupChanges(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	writeMinimalWorkspaceProvider(t, cfg, "coding", "true")
	session := &domain.Session{Workflow: "coding"}
	plan, err := buildPlanForSession(cfg, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}
	wsp, err := resolveSessionWorkspaceProvider(cfg, session)
	if err != nil {
		t.Fatalf("resolveSessionWorkspaceProvider: %v", err)
	}
	before, err := lifecycleConfigurationDigest(plan, nil, wsp)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	rewriteWorkspaceProviderCleanup(t, cfg, "coding", "echo changed")

	wsp, err = resolveSessionWorkspaceProvider(cfg, session)
	if err != nil {
		t.Fatalf("resolveSessionWorkspaceProvider: %v", err)
	}
	after, err := lifecycleConfigurationDigest(plan, nil, wsp)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (after): %v", err)
	}
	if before == after {
		t.Fatalf("digest unchanged (%q) after the workspace provider's cleanup action changed", before)
	}
}

// A nested task's `[outputs.bind]` entry decides what a dependent (or a
// later cleanup) reads as this task's public output, so it must be part of
// the projection even though it is not itself a setup/cleanup action.
func TestLifecycleConfigurationDigest_ChangesWhenNestedOutputBindChanges(t *testing.T) {
	before := nestedConfig(t, taskFixture{}, bindPid)
	after := nestedConfig(t, taskFixture{}, strings.ReplaceAll(bindPid, `"inner.outputs.pid"`, `"inner.outputs.pid2"`))
	session := &domain.Session{Workflow: "default"}

	beforePlan, err := buildPlanForSession(before, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession (before): %v", err)
	}
	beforeDigest, err := lifecycleConfigurationDigest(beforePlan, nil, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	afterPlan, err := buildPlanForSession(after, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession (after): %v", err)
	}
	afterDigest, err := lifecycleConfigurationDigest(afterPlan, nil, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (after): %v", err)
	}

	if beforeDigest == afterDigest {
		t.Fatalf("digest unchanged (%q) after the nested task's output bind changed", beforeDigest)
	}
}

// RunLayerCleanup reads a layer's BindOutputs to build that layer's own
// cleanup self view, so an outstanding execution outside the desired
// workflow (whose declaration is only ever visited through
// projectLayerCleanupOnly, never projectLayer) must still project them.
func TestLifecycleConfigurationDigest_ChangesWhenCleanupOnlyOutputBindChanges(t *testing.T) {
	nestedDefs := func(bindExtra string) []taskFixture {
		return []taskFixture{
			{id: "runtime", scope: contract.TaskScopeRun},
			{id: "team_runtime", scope: contract.TaskScopeRun, extra: "inner = \"runtime\"\n" + bindExtra},
			{id: "kept", scope: "run", setup: `echo '{}'`, cleanup: "true"},
		}
	}
	before := writeWorkflowFixture(t, t.TempDir(), "coding", nestedDefs(bindPid), []nodeFixture{{id: "kept"}})
	after := writeWorkflowFixture(t, t.TempDir(), "coding",
		nestedDefs(strings.ReplaceAll(bindPid, `"inner.outputs.pid"`, `"inner.outputs.pid2"`)),
		[]nodeFixture{{id: "kept"}},
	)
	session := &domain.Session{
		Workflow: "coding",
		Nodes: map[string]*contract.TaskState{
			"orphan": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "team_runtime", Seq: 1, Outputs: map[string]any{}},
		},
	}

	plan, err := buildPlanForSession(before, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}
	beforeOutstanding, err := unifiedTeardownList(before, session, false)
	if err != nil {
		t.Fatalf("unifiedTeardownList (before): %v", err)
	}
	beforeDigest, err := lifecycleConfigurationDigest(plan, beforeOutstanding, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	afterOutstanding, err := unifiedTeardownList(after, session, false)
	if err != nil {
		t.Fatalf("unifiedTeardownList (after): %v", err)
	}
	afterDigest, err := lifecycleConfigurationDigest(plan, afterOutstanding, resolvedWorkspaceProvider{})
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (after): %v", err)
	}

	if beforeDigest == afterDigest {
		t.Fatalf("digest unchanged (%q) after an outstanding-but-outside-the-workflow node's output bind changed", beforeDigest)
	}
}

// A missing definition is a precondition failure, not a configuration
// change: an operator repairing it still deserves the warning on the next
// run, not silence because an earlier attempt already recorded a baseline
// for configuration nothing actually used.
func TestDown_UnresolvedNodeDefinitionBlocksBaselineAdvance(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "kept", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "kept"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{
		"gone": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "gone", Seq: 1, Outputs: map[string]any{}},
	})

	if _, err := Down(cfg, store, DownParams{Identifier: "sess-1"}); err == nil {
		t.Fatal("Down: want the unresolved definition surfaced, got nil error")
	}
	if got := store.Get("sess-1").LifecycleConfigurationDigest; got != "" {
		t.Errorf("baseline = %q, want it left unrecorded when a precondition (missing definition) fails", got)
	}
}

// force-recreate re-runs workspace-provider setup (recreateSessionRuntime),
// so an invalid workspace_provider_inputs value is exactly as much a
// precondition failure there as a missing definition is for cleanup.
func TestUp_ForceRecreateProviderInputPreconditionBlocksBaselineAdvance(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	workspacesDir := mkdirWorkspaces(t, cfg)
	doc := "[coding_provider]\n" +
		"kind = \"workspace_provider\"\n\n" +
		"[coding_provider.inputs_schema]\n" +
		"type = \"object\"\n" +
		"required = [\"layout\"]\n\n" +
		"[coding_provider.setup]\n" +
		"type = \"shell\"\n" +
		"script = \"echo '{\\\"workspace_dir\\\":\\\".\\\"}'\"\n"
	if err := os.WriteFile(filepath.Join(workspacesDir, "coding_provider.toml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, "coding", "workspace_provider = \"coding_provider\"\n")
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1", ForceRecreate: true}); err == nil {
		t.Fatal("Up: want the invalid workspace_provider_inputs surfaced, got nil error")
	}
	if got := store.Get("sess-1").LifecycleConfigurationDigest; got != "" {
		t.Errorf("baseline = %q, want it left unrecorded when the workspace-provider-inputs precondition fails", got)
	}
}

// Up resolves a stale workflow node's cleanup once and must feed that same
// resolved list to the actual cleanup call rather than re-resolving it from
// current config a second time. A Resolved entry whose declaration exists
// nowhere on disk still runs successfully here, which only holds if the
// call uses the value handed to it instead of looking its TaskID back up.
func TestCleanupStaleWorkflowNodes_UsesProvidedResolvedListWithoutReResolving(t *testing.T) {
	requireBash(t)
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "kept", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "kept"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{
		"ghost": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "ghost_def_not_on_disk", Seq: 1},
	})
	session := store.Get("sess-1")
	plan, err := buildPlanForSession(cfg, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}
	stale := []task.Resolved{{
		NodeID:  "ghost",
		TaskID:  "ghost_def_not_on_disk",
		Scope:   contract.TaskScopeRun,
		Cleanup: &lang.Action{Type: lang.ActionShell, Script: "printf 'ghost-cleaned\\n' >> " + cleanupLog},
	}}

	if err := cleanupStaleWorkflowNodes(cfg, store, "sess-1", session, plan, stale, nil); err != nil {
		t.Fatalf("cleanupStaleWorkflowNodes: %v", err)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil || string(data) != "ghost-cleaned\n" {
		t.Fatalf("cleanup log = %q, %v, want the provided resolved cleanup to have run without re-resolving %q from disk", data, err, stale[0].TaskID)
	}
}

// The result's own warning field is not the only place the notice must
// reach an operator: a run that goes on to fail after the notice would
// otherwise carry it nowhere at all.
func TestDown_LogsChangedConfigurationEvenWhenExecutionFails(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "flaky", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "flaky"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})
	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Up (first): %v", err)
	}
	rewriteTaskFixture(t, cfg, taskFixture{id: "flaky", scope: "run", setup: `echo '{}'`, cleanup: "exit 1"})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	if _, err := Down(cfg, store, DownParams{Identifier: "sess-1"}); err == nil {
		t.Fatal("Down: want the now-failing cleanup's failure surfaced, got nil error")
	}
	if !strings.Contains(buf.String(), "lifecycle configuration has changed") {
		t.Fatalf("log output = %q, want the config-change warning logged even though execution failed", buf.String())
	}
}

// A structured MCP caller only ever sees the returned error, never process
// stderr, so the notice must also reach it through the error value itself.
func TestDown_ErrorCarriesChangedConfigurationWarningOnFailure(t *testing.T) {
	requireBash(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "flaky", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "flaky"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})
	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Up (first): %v", err)
	}
	rewriteTaskFixture(t, cfg, taskFixture{id: "flaky", scope: "run", setup: `echo '{}'`, cleanup: "exit 1"})

	_, err := Down(cfg, store, DownParams{Identifier: "sess-1"})
	if err == nil {
		t.Fatal("Down: want the now-failing cleanup's failure surfaced, got nil error")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T, want *Error", err)
	}
	if svcErr.LifecycleConfigurationWarning == "" {
		t.Fatal("Error.LifecycleConfigurationWarning is empty, want the config-change notice carried on the error itself")
	}
}

// The workspace provider is identified by which one the workflow selects,
// not only by that provider's own action content: two providers with
// identical setup/cleanup would otherwise be indistinguishable.
func TestLifecycleConfigurationDigest_ChangesWhenWorkspaceProviderReferenceChanges(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	writeWorkspaceProviderDoc(t, mkdirWorkspaces(t, cfg), "provider_a", "true")
	writeWorkspaceProviderDoc(t, filepath.Join(cfg.BaseDir, "workspaces"), "provider_b", "true")
	session := &domain.Session{Workflow: "coding"}

	addWorkflowFields(t, cfg, "coding", "workspace_provider = \"provider_a\"\n")
	planA, err := buildPlanForSession(cfg, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}
	wspA, err := resolveSessionWorkspaceProvider(cfg, session)
	if err != nil {
		t.Fatalf("resolveSessionWorkspaceProvider: %v", err)
	}
	digestA, err := lifecycleConfigurationDigest(planA, nil, wspA)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (provider_a): %v", err)
	}

	rewriteWorkflowWorkspaceProvider(t, cfg, "coding", "provider_a", "provider_b")
	planB, err := buildPlanForSession(cfg, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}
	wspB, err := resolveSessionWorkspaceProvider(cfg, session)
	if err != nil {
		t.Fatalf("resolveSessionWorkspaceProvider: %v", err)
	}
	digestB, err := lifecycleConfigurationDigest(planB, nil, wspB)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (provider_b): %v", err)
	}

	if digestA == digestB {
		t.Fatalf("digest unchanged (%q) after the workflow selected a different (identically-scripted) workspace provider", digestA)
	}
}

// wf.WorkspaceProviderInputs are declared literal values, part of the
// workflow's own configuration, not runtime data -- a change to one must
// change the digest even though no action's text changed at all.
func TestLifecycleConfigurationDigest_ChangesWhenWorkspaceProviderInputsChange(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "build", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "build"}},
	)
	workspacesDir := mkdirWorkspaces(t, cfg)
	doc := "[coding_provider]\n" +
		"kind = \"workspace_provider\"\n\n" +
		"[coding_provider.inputs_schema]\n" +
		"type = \"object\"\n\n" +
		"[coding_provider.setup]\n" +
		"type = \"shell\"\n" +
		"script = \"echo '{\\\"workspace_dir\\\":\\\".\\\"}'\"\n"
	if err := os.WriteFile(filepath.Join(workspacesDir, "coding_provider.toml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, "coding", "workspace_provider = \"coding_provider\"\n[coding.workspace_provider_inputs]\nlayout = 1\n")
	session := &domain.Session{Workflow: "coding"}

	plan, err := buildPlanForSession(cfg, "", session)
	if err != nil {
		t.Fatalf("buildPlanForSession: %v", err)
	}
	wsp, err := resolveSessionWorkspaceProvider(cfg, session)
	if err != nil {
		t.Fatalf("resolveSessionWorkspaceProvider: %v", err)
	}
	before, err := lifecycleConfigurationDigest(plan, nil, wsp)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	rewriteWorkflowFile(t, cfg, "coding", func(body string) string {
		return strings.Replace(body, "layout = 1", "layout = 2", 1)
	})

	wsp, err = resolveSessionWorkspaceProvider(cfg, session)
	if err != nil {
		t.Fatalf("resolveSessionWorkspaceProvider: %v", err)
	}
	after, err := lifecycleConfigurationDigest(plan, nil, wsp)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (after): %v", err)
	}
	if before == after {
		t.Fatalf("digest unchanged (%q) after workspace_provider_inputs changed", before)
	}
}

func mkdirWorkspaces(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func rewriteWorkflowWorkspaceProvider(t *testing.T, cfg *config.Config, wfID, fromProviderID, toProviderID string) {
	t.Helper()
	rewriteWorkflowFile(t, cfg, wfID, func(body string) string {
		return strings.Replace(body,
			"workspace_provider = \""+fromProviderID+"\"",
			"workspace_provider = \""+toProviderID+"\"",
			1)
	})
}

func rewriteWorkflowFile(t *testing.T, cfg *config.Config, wfID string, edit func(string) string) {
	t.Helper()
	path := filepath.Join(cfg.BaseDir, "workflows", wfID+".toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(edit(string(data))), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeMinimalWorkspaceProvider attaches a trivial workspace provider (shell
// setup/cleanup, no match/name -- an identity-workflow provider, valid per
// config.WorkspaceProviderConfig's own doc comment) to wfID, so a test can
// exercise it without a real plugin-backed provider.
func writeMinimalWorkspaceProvider(t *testing.T, cfg *config.Config, wfID, cleanupScript string) {
	t.Helper()
	workspacesDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(workspacesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	provID := wfID + "_provider"
	writeWorkspaceProviderDoc(t, workspacesDir, provID, cleanupScript)
	addWorkflowFields(t, cfg, wfID, "workspace_provider = \""+provID+"\"\n")
}

// rewriteWorkspaceProviderCleanup changes an already-attached minimal
// workspace provider's cleanup script in place.
func rewriteWorkspaceProviderCleanup(t *testing.T, cfg *config.Config, wfID, cleanupScript string) {
	t.Helper()
	writeWorkspaceProviderDoc(t, filepath.Join(cfg.BaseDir, "workspaces"), wfID+"_provider", cleanupScript)
}

func writeWorkspaceProviderDoc(t *testing.T, workspacesDir, provID, cleanupScript string) {
	t.Helper()
	doc := "[" + provID + "]\n" +
		"kind = \"workspace_provider\"\n\n" +
		"[" + provID + ".setup]\n" +
		"type = \"shell\"\n" +
		"script = \"echo '{\\\"workspace_dir\\\":\\\".\\\"}'\"\n\n" +
		"[" + provID + ".cleanup]\n" +
		"type = \"shell\"\n" +
		"script = " + `"` + cleanupScript + `"` + "\n"
	if err := os.WriteFile(filepath.Join(workspacesDir, provID+".toml"), []byte(doc), 0o644); err != nil {
		t.Fatalf("write workspace provider fixture %q: %v", provID, err)
	}
}
