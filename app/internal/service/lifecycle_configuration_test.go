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
	before, err := lifecycleConfigurationDigest(cfg, session, plan)
	if err != nil {
		t.Fatalf("lifecycleConfigurationDigest (before): %v", err)
	}

	rewriteWorkspaceProviderCleanup(t, cfg, "coding", "echo changed")

	after, err := lifecycleConfigurationDigest(cfg, session, plan)
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
		t.Fatalf("digest unchanged (%q) after the nested task's output bind changed", beforeDigest)
	}
}

// Acceptance: "Given a missing definition ... cleanup does not fall back or
// infer release" -- and, as a precondition failure rather than a
// configuration change, it must not advance the notification baseline
// either: an operator repairing the definition still deserves the warning
// the next time it runs, not silence because a prior attempt already
// (wrongly) recorded a baseline for configuration nothing actually used.
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
