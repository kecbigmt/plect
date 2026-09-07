package task

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// providerExec builds a provider hook whose subject is the hook's lifecycle
// rather than its value forms: one literal script, with the values it needs
// as positional arguments.
func providerExec(script string, args ...*lang.Value) *lang.Action {
	argv := []*lang.Value{
		{Form: lang.FormLiteral, Literal: "-c"},
		{Form: lang.FormLiteral, Literal: script},
		{Form: lang.FormLiteral, Literal: "provider"},
	}
	return &lang.Action{Type: lang.ActionExec, Command: "sh", Args: append(argv, args...)}
}

func literalValue(v string) *lang.Value { return &lang.Value{Form: lang.FormLiteral, Literal: v} }

func fromValue(path string) *lang.Value { return &lang.Value{Form: lang.FormFrom, From: path} }

func exprValue(src string) *lang.Value { return &lang.Value{Form: lang.FormExpr, Expr: src} }

func fromValueOr(path, fallback string) *lang.Value {
	return &lang.Value{Form: lang.FormFrom, From: path, Default: fallback, HasDefault: true}
}

// TestRunWorkflowSetup_ResolvesBinReference pins that a workspace provider's
// setup/cleanup hooks can invoke a plugin-shipped executable through
// `{{bin ...}}`, the same helper task setup/cleanup and resource observe
// hooks already get — a workspace provider hook that only has bare command
// names on `PATH` available cannot reliably invoke its own plugin's
// executables once that plugin is mounted read-only rather than installed
// onto `PATH`.
func TestRunWorkflowSetup_ResolvesBinReference(t *testing.T) {
	mounted := []plugins.Mounted{mustMount("official/github", "/mnt/github",
		plugins.Executable{Name: "github-worktree", Path: "bin/github-worktree"})}
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Setup: providerExec(`printf '{"workspace_dir":"/tmp/x","bin":"%s"}' "$1"`,
			&lang.Value{Form: lang.FormBin, Bin: "official/github/github-worktree"}),
	}
	tasks := map[string]*contract.TaskState{}
	outputs, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{ResourceID: "r", SessionName: "s", Plugins: mounted}, tasks, nil)
	if err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}
	want := filepath.Join("/mnt/github", "bin", "github-worktree")
	if outputs["bin"] != want {
		t.Errorf("bin = %v, want %q", outputs["bin"], want)
	}
}

func TestRunWorkflowSetup_ProducesWorkspaceDir(t *testing.T) {
	dir := t.TempDir()
	prov := config.WorkspaceProviderConfig{
		ID:    "wf",
		Setup: providerExec(`printf '{"workspace_dir":"%s","branch":"issue/1"}' "$1"`, literalValue(dir)),
	}
	tasks := map[string]*contract.TaskState{}
	outputs, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{ResourceID: "https://example.com/1", SessionName: "s"}, tasks, nil)
	if err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}
	if outputs["workspace_dir"] != dir {
		t.Errorf("workspace_dir = %v, want %s", outputs["workspace_dir"], dir)
	}
	st := tasks[contract.WorkflowPseudoNodeID]
	if st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("pseudo-node state = %+v, want produced", st)
	}
	if st.Scope != contract.TaskScopeSession {
		t.Errorf("scope = %q, want session", st.Scope)
	}
}

func TestRunWorkflowSetup_ReadsItsSurfaceRoots(t *testing.T) {
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Setup: providerExec(`printf '{"workspace_dir":"/tmp/x","resource":"%s","session":"%s","root":"%s","tpl":"%s"}' "$1" "$2" "$3" "$4"`,
			fromValue("resource.id"), fromValue("session.name"),
			fromValue("config.workspace_dirs_root"), fromValueOr("session.inputs.flavour", "")),
	}
	tasks := map[string]*contract.TaskState{}
	outputs, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{
		ResourceID:        "res-1",
		SessionName:       "sess-1",
		WorkspaceDirsRoot: "/roots/workspace_dirs",
		SessionInputs:     map[string]any{"flavour": "review"},
	}, tasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outputs["resource"] != "res-1" || outputs["session"] != "sess-1" || outputs["root"] != "/roots/workspace_dirs" || outputs["tpl"] != "review" {
		t.Errorf("setup did not observe its roots: %v", outputs)
	}
}

// An exec action passes a value as one argv element, never as command text,
// so a resource id carrying shell metacharacters reaches the invoked
// executable literally and its injected command never runs.
func TestRunWorkflowSetup_AValueIsNeverCommandText(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	out := filepath.Join(dir, "out")
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Setup: providerExec(`printf '%s' "$1" > "$2"; printf '{"workspace_dir":"%s","branch":"issue/1"}' "$3"`,
			fromValue("resource.id"), literalValue(out), literalValue(dir)),
	}
	malicious := `x"; touch ` + marker + `; echo "`
	tasks := map[string]*contract.TaskState{}
	if _, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{ResourceID: malicious, SessionName: "s"}, tasks, nil); err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the injected command executed: a value reached the shell as syntax")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if string(got) != malicious {
		t.Errorf("the executable received %q, want the literal resource id %q", got, malicious)
	}
}

func TestRunWorkflowSetup_MissingWorkspaceDirFails(t *testing.T) {
	prov := config.WorkspaceProviderConfig{ID: "wf", Setup: providerExec(`echo '{"branch":"b"}'`)}
	tasks := map[string]*contract.TaskState{}
	_, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err == nil {
		t.Fatal("expected error for missing workspace_dir output")
	}
	if !strings.Contains(err.Error(), "workspace_dir") {
		t.Errorf("unexpected message: %v", err)
	}
	st := tasks[contract.WorkflowPseudoNodeID]
	if st == nil || st.Status != contract.TaskStatusFailed {
		t.Errorf("pseudo-node should be failed, got %+v", st)
	}
}

func TestRunWorkflowSetup_IdempotentSkip(t *testing.T) {
	prov := config.WorkspaceProviderConfig{ID: "wf", Setup: providerExec(`echo should-not-run >&2; exit 1`)}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd"},
		},
	}
	outputs, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err != nil {
		t.Fatalf("produced pseudo-node must short-circuit: %v", err)
	}
	if outputs["workspace_dir"] != "/tmp/wd" {
		t.Errorf("outputs = %v", outputs)
	}
}

func TestRunWorkflowSetup_PrevSurvivesRetry(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "prev.txt")
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Setup: providerExec(`printf '%s' "$1" > "$2"; echo '{"workspace_dir":"/tmp/new"}'`,
			fromValueOr("prev.workspace_dir", ""), literalValue(marker)),
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusFailed,
			Outputs: map[string]any{"workspace_dir": "/tmp/old"},
		},
	}
	if _, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "/tmp/old" {
		t.Errorf("prev.workspace_dir = %q, want /tmp/old", strings.TrimSpace(string(data)))
	}
}

func TestRunWorkflowSetup_OutputsSchemaEnforced(t *testing.T) {
	prov := config.WorkspaceProviderConfig{
		ID:    "wf",
		Setup: providerExec(`echo '{"workspace_dir":"/tmp/x","branch":42}'`),
		OutputsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"branch": map[string]any{"type": "string"},
			},
		},
	}
	tasks := map[string]*contract.TaskState{}
	_, _, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err == nil {
		t.Fatal("expected schema violation")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("unexpected message: %v", err)
	}
}

// A produced provider record whose alive probe passes is reused: no cleanup,
// no fresh setup.
func TestRunWorkflowSetup_PassingAliveReusesWithoutCleanup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup-ran")
	prov := config.WorkspaceProviderConfig{
		ID:      "wf",
		Setup:   providerExec("echo should-not-run >&2; exit 1"),
		Cleanup: providerExec("touch " + marker),
		Health:  &config.HealthConfig{Alive: providerExec("exit 0")},
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd"},
		},
	}
	outputs, repaired, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}
	if repaired {
		t.Error("repaired = true, want false: a passing alive probe reuses the record")
	}
	if outputs["workspace_dir"] != "/tmp/wd" {
		t.Errorf("outputs = %v, want the reused record's outputs", outputs)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("cleanup ran even though the alive probe passed")
	}
}

// A noop alive probe reuses the produced record without running anything —
// not even the probe itself.
func TestRunWorkflowSetup_NoopAliveReusesWithoutExecutingAnything(t *testing.T) {
	prov := config.WorkspaceProviderConfig{
		ID:     "wf",
		Setup:  providerExec("echo should-not-run >&2; exit 1"),
		Health: &config.HealthConfig{Alive: &lang.Action{Type: lang.ActionNoop}},
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd"},
		},
	}
	outputs, repaired, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}
	if repaired {
		t.Error("repaired = true, want false: a noop probe never runs a check to fail")
	}
	if outputs["workspace_dir"] != "/tmp/wd" {
		t.Errorf("outputs = %v, want the reused record's outputs", outputs)
	}
}

// A failed alive probe repairs the record: cleanup runs with force=true,
// then setup runs again and its fresh outputs replace the stale ones.
func TestRunWorkflowSetup_FailingAliveRepairsViaForcedCleanupThenSetup(t *testing.T) {
	dir := t.TempDir()
	cleanupMarker := filepath.Join(dir, "cleanup-force")
	prov := config.WorkspaceProviderConfig{
		ID:    "wf",
		Setup: providerExec(`echo '{"workspace_dir":"/tmp/new"}'`),
		Cleanup: providerExec(`printf '%s' "$1" > "$2"`,
			fromValue("force"), literalValue(cleanupMarker)),
		Health: &config.HealthConfig{Alive: providerExec("exit 1")},
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/old"},
		},
	}
	outputs, repaired, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}
	if !repaired {
		t.Error("repaired = false, want true: the alive probe failed")
	}
	if outputs["workspace_dir"] != "/tmp/new" {
		t.Errorf("outputs = %v, want the rebuilt record", outputs)
	}
	got, readErr := os.ReadFile(cleanupMarker)
	if readErr != nil {
		t.Fatalf("read cleanup marker: %v", readErr)
	}
	if strings.TrimSpace(string(got)) != "true" {
		t.Errorf("cleanup's force root = %q, want true: repair always forces cleanup", got)
	}
	st := tasks[contract.WorkflowPseudoNodeID]
	if st.Status != contract.TaskStatusProduced || st.Outputs["workspace_dir"] != "/tmp/new" {
		t.Errorf("pseudo-node state = %+v, want produced with the rebuilt outputs", st)
	}
}

// The alive probe observes its own recorded outputs, the provider's
// resolved inputs, the session name, and the configured workspace-dirs
// root — the same roots cleanup does, minus force and cleanup.inputs.
func TestRunWorkflowSetup_AliveProbeReadsItsSurfaceRoots(t *testing.T) {
	prov := config.WorkspaceProviderConfig{
		ID:    "wf",
		Setup: providerExec("echo should-not-run >&2; exit 1"),
		Health: &config.HealthConfig{Alive: providerExec(
			`test "$1" = "/tmp/old" && test "$2" = "review" && test "$3" = "sess-1" && test "$4" = "/roots"`,
			fromValue("self.outputs.workspace_dir"), fromValue("inputs.flavour"),
			fromValue("session.name"), fromValue("config.workspace_dirs_root"),
		)},
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/old"},
		},
	}
	vars := effect.WorkflowHookVars{
		SessionName:       "sess-1",
		WorkspaceDirsRoot: "/roots",
		Inputs:            map[string]any{"flavour": "review"},
	}
	if _, repaired, err := RunWorkflowSetup(prov, vars, tasks, nil); err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	} else if repaired {
		t.Error("repaired = true, want false: the probe observed its expected roots and passed")
	}
}

// A cleanup failure during repair stops the whole attempt without ever
// running setup, and the persisted state carries the cleanup's own failure
// rather than the alive check's.
func TestRunWorkflowSetup_FailingAliveRepairCleanupFailureStopsBeforeSetup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "setup-ran")
	prov := config.WorkspaceProviderConfig{
		ID:      "wf",
		Setup:   providerExec("touch " + marker + "; echo '{\"workspace_dir\":\"/tmp/new\"}'"),
		Cleanup: providerExec("exit 7"),
		Health:  &config.HealthConfig{Alive: providerExec("exit 1")},
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/old"},
		},
	}
	_, repaired, err := RunWorkflowSetup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err == nil {
		t.Fatal("expected the repair's forced cleanup failure to stop the attempt")
	}
	if repaired {
		t.Error("repaired = true, want false: a failed repair never completes")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("setup ran even though the repair's forced cleanup failed")
	}
	st := tasks[contract.WorkflowPseudoNodeID]
	if st.Status != contract.TaskStatusFailed {
		t.Fatalf("pseudo-node status = %q, want failed", st.Status)
	}
}

func TestRunWorkflowCleanup_RunsAndMarksClean(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned.txt")
	prov := config.WorkspaceProviderConfig{
		ID:      "wf",
		Setup:   providerExec(`echo '{}'`),
		Cleanup: providerExec(`printf '%s' "$1" > "$2"`, fromValue("self.outputs.workspace_dir"), literalValue(marker)),
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd"},
		},
	}
	if err := RunWorkflowCleanup(prov, effect.WorkflowHookVars{}, tasks, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "/tmp/wd" {
		t.Errorf("self.outputs.workspace_dir = %q", strings.TrimSpace(string(data)))
	}
	st := tasks[contract.WorkflowPseudoNodeID]
	if st.Status != contract.TaskStatusCleaned {
		t.Errorf("status = %q, want cleaned", st.Status)
	}
	if st.Outputs["workspace_dir"] != "/tmp/wd" {
		t.Error("outputs must survive cleanup, so a later setup retry can read prev.*")
	}
}

// Core forwards a caller's cleanup intents opaquely, so a new one never
// needs a core vocabulary addition.
func TestRunWorkflowCleanup_CleanupInputsReachTheAction(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "delete_branch.txt")
	prov := config.WorkspaceProviderConfig{
		ID:    "wf",
		Setup: providerExec(`echo '{}'`),
		Cleanup: providerExec(`printf '%s' "$1" > "$2"`,
			fromValueOr("cleanup.inputs.delete_branch", ""), literalValue(marker)),
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd"},
		},
	}
	vars := effect.WorkflowHookVars{CleanupInputs: map[string]string{"delete_branch": "true"}}
	if err := RunWorkflowCleanup(prov, vars, tasks, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "true" {
		t.Errorf("cleanup.inputs.delete_branch resolved to %q, want true", got)
	}
}

// An intent no caller set resolves through the value's own default rather
// than failing the hook, so a cleanup action reading one key never has to
// special-case "no intents given".
func TestRunWorkflowCleanup_CleanupInputsDefaultUnset(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "delete_branch.txt")
	prov := config.WorkspaceProviderConfig{
		ID:    "wf",
		Setup: providerExec(`echo '{}'`),
		Cleanup: providerExec(`printf '%s' "$1" > "$2"`,
			fromValueOr("cleanup.inputs.delete_branch", ""), literalValue(marker)),
	}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd"},
		},
	}
	if err := RunWorkflowCleanup(prov, effect.WorkflowHookVars{}, tasks, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "" {
		t.Errorf("cleanup.inputs.delete_branch resolved to %q, want the declared default when unset", got)
	}
}

func TestRunWorkflowCleanup_NoStateSkips(t *testing.T) {
	prov := config.WorkspaceProviderConfig{ID: "wf", Cleanup: providerExec("exit 1")}
	if err := RunWorkflowCleanup(prov, effect.WorkflowHookVars{}, map[string]*contract.TaskState{}, nil); err != nil {
		t.Fatalf("no setup state must skip cleanly: %v", err)
	}
}

func TestRunWorkflowCleanup_FailureMarksFailed(t *testing.T) {
	prov := config.WorkspaceProviderConfig{ID: "wf", Cleanup: providerExec("exit 7")}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd"},
		},
	}
	err := RunWorkflowCleanup(prov, effect.WorkflowHookVars{}, tasks, nil)
	if err == nil {
		t.Fatal("expected cleanup failure")
	}
	if tasks[contract.WorkflowPseudoNodeID].Status != contract.TaskStatusFailed {
		t.Errorf("status = %q, want failed", tasks[contract.WorkflowPseudoNodeID].Status)
	}
}

// A task node can reference the pseudo-node's outputs.
func TestRunSetup_ExposesWorkflowOutputs(t *testing.T) {
	resolved := []Resolved{{
		NodeID: "echoer",
		Scope:  config.TaskScopeSession,
		Setup: &lang.Action{
			Type:   lang.ActionShell,
			Script: `jq -nc --arg got "$branch" '{got:$got}'`,
			Bind:   map[string]*lang.Value{"branch": fromValue("workflow.outputs.branch")},
		},
		Inputs: map[string]*lang.Value{"wd": fromValue("workflow.outputs.workspace_dir")},
	}}
	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/wd", "branch": "issue/9"},
		},
	}
	if err := RunSetup(context.Background(), resolved, SessionVars{}, tasks, nil); err != nil {
		t.Fatal(err)
	}
	st := tasks["echoer"]
	if st.Outputs["got"] != "issue/9" {
		t.Errorf("workflow.outputs.branch = %v", st.Outputs["got"])
	}
	if st.Inputs["wd"] != "/tmp/wd" {
		t.Errorf("input binding over .Workflow.outputs.workspace_dir = %v", st.Inputs["wd"])
	}
}
