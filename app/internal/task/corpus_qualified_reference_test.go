package task

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// The conformance corpus states what the language accepts; on its own it says
// nothing about whether a session can run it. This loads the qualified-
// reference fixture verbatim as user-owned config, against plugins declaring
// exactly the definitions it names, and compiles the plan — so the fixture is
// exercised by the same path a session takes, not only by validation.
func TestCorpusFixture_QualifiedReferencesCompile(t *testing.T) {
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

	// One directory per plugin the fixture addresses, each declaring the id the
	// fixture names under that plugin's path. tmux is the real shipped plugin:
	// its pane effect answers to official.tmux.pane, the address the fixture
	// names, so no synthetic stand-in is needed for it.
	repoRoot := repoRootForTest(t)
	githubDir, claudeDir := t.TempDir(), t.TempDir()
	tmux := mountShippedPlugin(t, repoRoot, "official", "plugins/tmux")
	write(filepath.Join(githubDir, "config", "workspaces", "worktree.toml"), `
[worktree]
kind = "workspace_provider"

[worktree.setup]
type    = "exec"
command = "true"

[worktree.health.alive]
type = "noop"
`)
	write(filepath.Join(claudeDir, "config", "tasks", "runtime.toml"), `
[runtime]
kind  = "effect"
scope = "run"

[runtime.setup]
type   = "shell"
script = "echo runtime"

[runtime.health.alive]
type = "noop"

[runtime.inputs_schema]
tmux_session = { type = "string" }

[runtime.outputs_schema]
socket_path = { type = "string" }
`)
	write(filepath.Join(claudeDir, "config", "channels", "delivery.toml"), `
[delivery]
kind = "channel"
type = "unix_socket"
path = { from = "inputs.path" }
body = { json = { from = "event" } }

[delivery.input_schema]
path = { type = "string", required = true }
`)

	fixture, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "config-language", "references", "qualified.toml"))
	if err != nil {
		t.Fatalf("read corpus fixture: %v", err)
	}
	write(filepath.Join(base, "workflows", "qualified.toml"), string(fixture))

	cfg := &config.Config{
		BaseDir:    base,
		PluginDirs: []string{githubDir, tmux.Dir, claudeDir},
		Plugins: []plugins.Mounted{
			{ID: "official/github", Dir: githubDir},
			tmux,
			{ID: "official/claude", Dir: claudeDir},
		},
	}

	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	wf, ok := workflows["my_review"]
	if !ok {
		t.Fatalf("my_review not loaded: %v", config.Addresses(workflows))
	}
	defs, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	plan, err := CompileWorkflow(wf, defs)
	if err != nil {
		t.Fatalf("CompileWorkflow: %v", err)
	}
	// The fixture leaves the first node's id to default, and projects
	// `nodes.pane.outputs.*` from the second — so the defaulted id is the
	// reference's last segment, and the dependency edge it creates resolves.
	var names []string
	for _, node := range plan.Run {
		names = append(names, node.NodeID)
	}
	if len(names) != 2 || names[0] != "pane" || names[1] != "agent" {
		t.Fatalf("plan run nodes = %v, want pane then agent", names)
	}

	providers, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("LoadWorkspaceProviders: %v", err)
	}
	if _, ok := providers[wf.WorkspaceProvider]; !ok {
		t.Errorf("workspace provider %q unresolved among %v", wf.WorkspaceProvider, config.Addresses(providers))
	}
	channels, err := cfg.LoadChannels()
	if err != nil {
		t.Fatalf("LoadChannels: %v", err)
	}
	if err := config.ValidateWorkflowChannels(wf, channels); err != nil {
		t.Errorf("ValidateWorkflowChannels: %v", err)
	}
}
