package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// qualifiedReferenceConfig mounts one catalog plugin declaring an effect, a
// channel and a workspace provider, and a user-owned workflow that selects all
// three by their catalog addresses.
func qualifiedReferenceConfig(t *testing.T) *config.Config {
	t.Helper()
	pluginDir := t.TempDir()
	base := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pluginDir, "config", "tasks", "runtime.toml"), `
[runtime]
kind  = "effect"
scope = "run"

[runtime.setup]
type   = "shell"
script = "echo plugin-effect"

[runtime.health.alive]
type = "noop"
`)
	write(filepath.Join(pluginDir, "config", "channels", "delivery.toml"), `
[delivery]
kind    = "channel"
type    = "exec"
command = "true"
`)
	write(filepath.Join(pluginDir, "config", "workspaces", "worktree.toml"), `
[worktree]
kind = "workspace_provider"

[worktree.setup]
type    = "exec"
command = "true"

[worktree.health.alive]
type = "noop"
`)
	write(filepath.Join(base, "workflows", "review.toml"), `
[review]
kind               = "workflow"
workspace_provider = "official.acme.worktree"

[[review.nodes]]
uses = "official.acme.runtime"

[[review.event.channel]]
name    = "runtime"
uses    = "official.acme.delivery"
include = ["user.emit"]
`)
	return &config.Config{
		BaseDir:    base,
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
}

// A node's qualified `uses` reaches the plugin's effect through the same
// lookup a session's plan is compiled from.
func TestCompileWorkflow_QualifiedNodeUsesResolves(t *testing.T) {
	cfg := qualifiedReferenceConfig(t)
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	defs, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	wf, ok := workflows["review"]
	if !ok {
		t.Fatalf("review workflow not loaded: %v", workflows)
	}
	plan, err := CompileWorkflow(wf, defs)
	if err != nil {
		t.Fatalf("CompileWorkflow: %v", err)
	}
	// The node id defaults to the referenced definition's id, not the whole
	// dotted reference.
	if len(plan.Run) != 1 || plan.Run[0].NodeID != "runtime" {
		t.Fatalf("plan run nodes = %+v, want one node named runtime", plan.Run)
	}
	if got := plan.Run[0].Setup.Source(); !strings.Contains(got, "plugin-effect") {
		t.Errorf("node resolved to %q, want the plugin's own effect", got)
	}
}

// An event channel's qualified `uses` resolves against the loaded channels.
func TestValidateWorkflowChannels_QualifiedUsesResolves(t *testing.T) {
	cfg := qualifiedReferenceConfig(t)
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	channels, err := cfg.LoadChannels()
	if err != nil {
		t.Fatalf("LoadChannels: %v", err)
	}
	if err := config.ValidateWorkflowChannels(workflows["review"], channels); err != nil {
		t.Fatalf("ValidateWorkflowChannels: %v", err)
	}
}

// A workflow's qualified `workspace_provider` resolves against the loaded
// providers.
func TestLoadWorkspaceProviders_QualifiedWorkspaceProviderResolves(t *testing.T) {
	cfg := qualifiedReferenceConfig(t)
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	providers, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("LoadWorkspaceProviders: %v", err)
	}
	if _, ok := providers[workflows["review"].WorkspaceProvider]; !ok {
		t.Fatalf("workspace provider %q not found among %v", workflows["review"].WorkspaceProvider, config.Addresses(providers))
	}
}

// A user-owned reference that names a plugin's declaration by its bare id
// misses, and the failure says which address it meant.
func TestCompileWorkflow_BareReferenceToPluginContentIsNamed(t *testing.T) {
	cfg := qualifiedReferenceConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "workflows", "review.toml"), []byte(`
[review]
kind = "workflow"

[[review.nodes]]
uses = "runtime"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	defs, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	_, err = CompileWorkflow(workflows["review"], defs)
	if err == nil {
		t.Fatal("expected a bare reference to a plugin's effect to fail")
	}
	if !strings.Contains(err.Error(), "official.acme.runtime") {
		t.Errorf("error = %v, want it to name the address the reference meant", err)
	}
}
