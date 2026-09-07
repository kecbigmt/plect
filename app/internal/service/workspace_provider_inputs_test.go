package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func githubLikeProvider() config.WorkspaceProviderConfig {
	return config.WorkspaceProviderConfig{
		ID: "github",
		InputsSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"workspace_layout_root": map[string]any{"type": "string"},
				"delete_branch_default": map[string]any{"type": "string", "enum": []any{"true", "false"}},
			},
		},
	}
}

func TestResolveWorkspaceProviderInputs_AcceptsDeclaredKeys(t *testing.T) {
	wf := config.WorkflowFile{ID: "issue", WorkspaceProviderInputs: map[string]any{
		"workspace_layout_root": "~/worktrees",
		"delete_branch_default": "true",
	}}
	got, err := resolveWorkspaceProviderInputs(githubLikeProvider(), wf)
	if err != nil {
		t.Fatalf("resolveWorkspaceProviderInputs: %v", err)
	}
	if got["workspace_layout_root"] != "~/worktrees" || got["delete_branch_default"] != "true" {
		t.Errorf("inputs = %v", got)
	}
}

func TestResolveWorkspaceProviderInputs_RejectsUndeclaredKey(t *testing.T) {
	wf := config.WorkflowFile{ID: "issue", WorkspaceProviderInputs: map[string]any{"layout_root": "~/worktrees"}}
	_, err := resolveWorkspaceProviderInputs(githubLikeProvider(), wf)
	if err == nil {
		t.Fatal("expected an undeclared parameter to be rejected")
	}
}

func TestResolveWorkspaceProviderInputs_RejectsValueOutsideDeclaredRange(t *testing.T) {
	wf := config.WorkflowFile{ID: "issue", WorkspaceProviderInputs: map[string]any{"delete_branch_default": "yes"}}
	_, err := resolveWorkspaceProviderInputs(githubLikeProvider(), wf)
	if err == nil {
		t.Fatal("expected a value outside the declared enum to be rejected")
	}
}

// A provider with no declared parameters has no variation point, so wiring one
// is an author error rather than a value silently dropped on the floor.
func TestResolveWorkspaceProviderInputs_RejectsInputsWithoutSchema(t *testing.T) {
	prov := config.WorkspaceProviderConfig{ID: "plain"}
	wf := config.WorkflowFile{ID: "issue", WorkspaceProviderInputs: map[string]any{"anything": "x"}}
	_, err := resolveWorkspaceProviderInputs(prov, wf)
	if err == nil {
		t.Fatal("expected inputs against a schema-less provider to be rejected")
	}
	if !strings.Contains(err.Error(), "inputs_schema") {
		t.Errorf("error = %v, want it to name inputs_schema", err)
	}
}

func TestResolveWorkspaceProviderInputs_SchemaLessProviderWithNoInputs(t *testing.T) {
	got, err := resolveWorkspaceProviderInputs(config.WorkspaceProviderConfig{ID: "plain"}, config.WorkflowFile{ID: "issue"})
	if err != nil {
		t.Fatalf("resolveWorkspaceProviderInputs: %v", err)
	}
	if got != nil {
		t.Errorf("inputs = %v, want nil", got)
	}
}

// TestWorkspaceProviderInputs_FromTOMLReachTheHook is the end-to-end guard for
// the key a workflow author actually types: it starts from the two config
// files on disk and ends at the rendered hook, so a decoding mistake between
// them — a mis-tagged `workspace_provider_inputs`, a parameter that never
// reaches `.Inputs` — fails here rather than silently doing nothing.
func TestWorkspaceProviderInputs_FromTOMLReachTheHook(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	writeFile(t, filepath.Join(globalDir, "config.toml"), "schema_version = 2\n")
	writeFile(t, filepath.Join(globalDir, "workspaces", "demo.toml"), `
[demo_provider]
kind = "workspace_provider"

[demo_provider.setup]
type    = "exec"
command = "printf"
args    = ['{"workspace_dir":"%s"}', { from = "inputs.layout_root", default = "" }]

[demo_provider.health.alive]
type = "noop"

[demo_provider.inputs_schema]
type                 = "object"
additionalProperties = false

[demo_provider.inputs_schema.properties]
layout_root = { type = "string" }
`)
	writeFile(t, filepath.Join(globalDir, "workflows", "demo.toml"), `
[demo]
kind = "workflow"
workspace_provider = "demo_provider"

[demo.workspace_provider_inputs]
layout_root = "~/worktrees"

[[demo.nodes]]
uses = "noop"
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	wf, ok := workflows["demo"]
	if !ok {
		t.Fatal("workflow demo not loaded")
	}
	providers, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("LoadWorkspaceProviders: %v", err)
	}
	prov, ok := providers["demo_provider"]
	if !ok {
		t.Fatal("workspace provider demo_provider not loaded")
	}

	inputs, err := resolveWorkspaceProviderInputs(prov, wf)
	if err != nil {
		t.Fatalf("resolveWorkspaceProviderInputs: %v", err)
	}
	if inputs["layout_root"] != "~/worktrees" {
		t.Fatalf("inputs = %v, want the value the workflow file declared", inputs)
	}

	// The hook is actually run, not just rendered: it echoes the parameter
	// back as its `workspace_dir` output, so the assertion below fails unless
	// the value survived every step from the workflow file to the script.
	outputs, _, err := task.RunWorkflowSetup(prov, effect.WorkflowHookVars{
		ResourceID:  "demo:1",
		SessionName: "demo-1",
		Inputs:      inputs,
		SourcePath:  prov.SourcePath,
	}, map[string]*contract.TaskState{}, nil)
	if err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}
	if outputs["workspace_dir"] != "~/worktrees" {
		t.Errorf("setup hook did not receive the parameter: outputs = %v", outputs)
	}
}

// A workflow that names a parameter the workspace provider never declared is
// rejected when the session resolves them, not discarded — the same reason
// additionalProperties is closed on the shipped provider's schema.
func TestWorkspaceProviderInputs_FromTOMLRejectsAnUndeclaredParameter(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	writeFile(t, filepath.Join(globalDir, "config.toml"), "schema_version = 2\n")
	writeFile(t, filepath.Join(globalDir, "workspaces", "demo.toml"), `
[demo_provider]
kind = "workspace_provider"

[demo_provider.setup]
type    = "exec"
command = "acquire"

[demo_provider.health.alive]
type = "noop"

[demo_provider.inputs_schema]
type                 = "object"
additionalProperties = false

[demo_provider.inputs_schema.properties]
layout_root = { type = "string" }
`)
	writeFile(t, filepath.Join(globalDir, "workflows", "demo.toml"), `
[demo]
kind = "workflow"
workspace_provider = "demo_provider"

[demo.workspace_provider_inputs]
laoyut_root = "~/worktrees"

[[demo.nodes]]
uses = "noop"
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	providers, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("LoadWorkspaceProviders: %v", err)
	}
	if _, err := resolveWorkspaceProviderInputs(providers["demo_provider"], workflows["demo"]); err == nil {
		t.Fatal("expected a misspelled parameter to be rejected")
	}
}
