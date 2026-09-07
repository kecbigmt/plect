package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// repoRoot locates the repository root from this test file's own path, so a
// test that reads a shipped config file does not depend on the working
// directory it is run from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// loadShippedWorkspaceProvider loads a workspace provider config that ships
// with a plugin, exercising the same loader the CLI uses at runtime. It
// mounts the plugin (not just PluginDirs) so a bare {{bin ...}} reference
// inside the shipped workspace provider file resolves the same way it would
// under a real catalog mount.
func loadShippedWorkspaceProvider(t *testing.T, pluginDir, id string) config.WorkspaceProviderConfig {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "plugins", pluginDir)
	m, err := plugins.LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest(%s): %v", pluginDir, err)
	}
	cfg := &config.Config{
		PluginDirs: []string{dir},
		Plugins:    []plugins.Mounted{{ID: "official/" + pluginDir, Dir: dir, Manifest: m}},
	}
	provs, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("load shipped workspace providers: %v", err)
	}
	// A mounted plugin's declaration answers to its catalog address, which is
	// what the mount above decides.
	address := "official." + pluginDir + "." + id
	prov, ok := provs[address]
	if !ok {
		t.Fatalf("shipped workspace provider %q not found", address)
	}
	return prov
}

// The shipped provider's resolver derives a session id from a resource
// identifier with a regular expression and a computation over its captures
// alone — core never asks a network for a name.
func TestShippedWorkspaceProvider_ResolvesResourceIdentifiersOffline(t *testing.T) {
	prov := loadShippedWorkspaceProvider(t, "github", "worktree")
	if !prov.HasResolver() {
		t.Fatal("the shipped workspace provider must declare a resolver")
	}

	tests := []struct {
		name     string
		resource string
		want     string
		matched  bool
	}{
		{"issue url", "https://github.com/acme/widgets/issues/42", "acme/widgets-42", true},
		{"pull request url", "https://github.com/acme/widgets/pull/7", "acme/widgets-7", true},
		{"issue url with trailing path", "https://github.com/acme/widgets/issues/42#issuecomment-1", "acme/widgets-42", true},
		{"unrelated url", "https://example.test/acme/widgets/items/42", "", false},
		{"bare identifier", "acme/widgets-42", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched, err := tryResolveName(prov, tt.resource)
			if err != nil {
				t.Fatalf("tryResolveName: %v", err)
			}
			if matched != tt.matched {
				t.Fatalf("matched = %v, want %v", matched, tt.matched)
			}
			if got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestShippedWorkspaceProvider_SetupHookDoesNotShellInjectResourceID pins
// that a resource id carrying shell metacharacters is passed to the setup
// hook literally rather than being interpreted by the shell that renders it.
// The injected command runs (if unescaped) regardless of whether
// github-worktree is even on PATH, since bash executes every
// semicolon-separated command in "cmd1; cmd2" independent of cmd1's exit
// status — so this test needs no built binaries.
func TestShippedWorkspaceProvider_SetupHookDoesNotShellInjectResourceID(t *testing.T) {
	prov := loadShippedWorkspaceProvider(t, "github", "worktree")
	marker := filepath.Join(t.TempDir(), "pwned")
	malicious := `https://github.com/acme/widgets/issues/1"; touch ` + marker + `; echo "`

	tasks := map[string]*contract.TaskState{}
	vars := effect.WorkflowHookVars{ResourceID: malicious, SessionName: "acme/widgets-1"}
	// Setup is expected to fail (github-worktree is not on PATH in this
	// test and the resource id is not a valid URL); only the absence of the
	// injected side effect is under test.
	_, _, _ = task.RunWorkflowSetup(prov, vars, tasks, nil)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the injected command executed: a resource id reached the shell as syntax")
	}
}

// The cleanup case mirrors the setup one. Cleanup binds the persisted
// workspace directory and branch — sourced from setup's own outputs, which in
// turn derive from the session's tag, so not fully outside user control.
func TestShippedWorkspaceProvider_CleanupHookDoesNotShellInjectWorkspaceDirOrBranch(t *testing.T) {
	prov := loadShippedWorkspaceProvider(t, "github", "worktree")
	marker := filepath.Join(t.TempDir(), "pwned")
	maliciousBranch := `issue/1"; touch ` + marker + `; echo "`

	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"workspace_dir": "/tmp/does-not-matter", "branch": maliciousBranch},
		},
	}
	vars := effect.WorkflowHookVars{ResourceID: "https://github.com/acme/widgets/issues/1", SessionName: "acme/widgets-1"}
	_ = task.RunWorkflowCleanup(prov, vars, tasks, nil)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the injected command executed: a recorded branch reached the shell as syntax")
	}
}

// TestShippedWorkspaceProvider_DeclaresAcquisitionAndRelease pins that the
// shipped workspace provider owns both halves of the workspace lifecycle,
// since a setup with no matching cleanup would strand every workspace
// directory it creates.
func TestShippedWorkspaceProvider_DeclaresAcquisitionAndRelease(t *testing.T) {
	prov := loadShippedWorkspaceProvider(t, "github", "worktree")
	if prov.Setup == nil {
		t.Error("the shipped workspace provider must declare setup")
	}
	if prov.Cleanup == nil {
		t.Error("the shipped workspace provider must declare cleanup")
	}
}

// TestShippedSlackThreadProvider_ResolvesResourceIdentifiersOffline pins that
// both permalink forms for one thread resolve to the same session name.
func TestShippedSlackThreadProvider_ResolvesResourceIdentifiersOffline(t *testing.T) {
	prov := loadShippedWorkspaceProvider(t, "slack", "thread_workspace")
	if !prov.HasResolver() {
		t.Fatal("the shipped workspace provider must declare a resolver")
	}

	tests := []struct {
		name     string
		resource string
		want     string
		matched  bool
	}{
		{
			"root permalink",
			"https://acme.slack.com/archives/C01ABCDEF/p1788226930843789",
			"slack/C01ABCDEF-1788226930843789",
			true,
		},
		{
			"reply permalink carrying the root thread_ts",
			"https://acme.slack.com/archives/C01ABCDEF/p1788227011000100?thread_ts=1788226930.843789&cid=C01ABCDEF",
			"slack/C01ABCDEF-1788226930843789",
			true,
		},
		{"unrelated url", "https://example.test/archives/C01ABCDEF/p1788226930843789", "", false},
		{"bare identifier", "slack/C01ABCDEF-1788226930843789", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched, err := tryResolveName(prov, tt.resource)
			if err != nil {
				t.Fatalf("tryResolveName: %v", err)
			}
			if matched != tt.matched {
				t.Fatalf("matched = %v, want %v", matched, tt.matched)
			}
			if got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestShippedSlackThreadProvider_HasNoSubscriptionRegistry pins that this
// provider declares no subscribe/unsubscribe hook: the slack plugin owns no
// watcher for a thread session to bind to.
func TestShippedSlackThreadProvider_HasNoSubscriptionRegistry(t *testing.T) {
	prov := loadShippedWorkspaceProvider(t, "slack", "thread_workspace")
	if prov.Setup == nil {
		t.Error("the shipped workspace provider must declare setup")
	}
	if prov.Cleanup == nil {
		t.Error("the shipped workspace provider must declare cleanup")
	}
	if prov.Subscribe != nil {
		t.Error("the shipped slack thread provider must declare no subscribe hook")
	}
	if prov.Unsubscribe != nil {
		t.Error("the shipped slack thread provider must declare no unsubscribe hook")
	}
}

// TestShippedSlackThreadProvider_SetupCreatesWorkspace runs the shipped
// setup/cleanup scripts themselves, not a stand-in for them.
func TestShippedSlackThreadProvider_SetupCreatesWorkspace(t *testing.T) {
	prov := loadShippedWorkspaceProvider(t, "slack", "thread_workspace")

	workspaceDirsRoot := t.TempDir()
	const (
		permalink   = "https://acme.slack.com/archives/C01ABCDEF/p1788226930843789"
		sessionName = "slack/C01ABCDEF-1788226930843789"
		channelID   = "C01ABCDEF"
		threadTS    = "1788226930.843789"
	)
	vars := effect.WorkflowHookVars{ResourceID: permalink, SessionName: sessionName, WorkspaceDirsRoot: workspaceDirsRoot}

	outputs, _, err := task.RunWorkflowSetup(prov, vars, map[string]*contract.TaskState{}, nil)
	if err != nil {
		t.Fatalf("RunWorkflowSetup: %v", err)
	}

	wantOutputs := map[string]any{
		"workspace_dir": filepath.Join(workspaceDirsRoot, "slack", channelID, threadTS),
		"channel_id":    channelID,
		"thread_ts":     threadTS,
		"permalink":     permalink,
	}
	for key, want := range wantOutputs {
		if outputs[key] != want {
			t.Errorf("outputs[%q] = %v, want %v", key, outputs[key], want)
		}
	}
	workspaceDir, _ := outputs["workspace_dir"].(string)
	if info, statErr := os.Stat(workspaceDir); statErr != nil || !info.IsDir() {
		t.Fatalf("setup did not create %q as a directory: %v", workspaceDir, statErr)
	}

	tasks := map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: outputs},
	}
	if err := task.RunWorkflowCleanup(prov, vars, tasks, nil); err != nil {
		t.Fatalf("RunWorkflowCleanup: %v", err)
	}
	if _, statErr := os.Stat(workspaceDir); !os.IsNotExist(statErr) {
		t.Fatalf("cleanup did not remove %q: %v", workspaceDir, statErr)
	}
}

// TestShippedSlackThreadProvider_SetupToleratesJSONSpecialCharactersInWorkspaceDirsRoot
// pins that a workspace_dirs_root byte outside what `match` constrains still
// round-trips through setup's outputs JSON.
func TestShippedSlackThreadProvider_SetupToleratesJSONSpecialCharactersInWorkspaceDirsRoot(t *testing.T) {
	tests := []struct {
		name string
		root string
	}{
		{"quote and backslash", `we"ird\root`},
		{"newline", "weird\nroot"},
		{"other control bytes", "weird\t\x01\x1froot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov := loadShippedWorkspaceProvider(t, "slack", "thread_workspace")

			workspaceDirsRoot := filepath.Join(t.TempDir(), tt.root)
			vars := effect.WorkflowHookVars{
				ResourceID:        "https://acme.slack.com/archives/C01ABCDEF/p1788226930843789",
				SessionName:       "slack/C01ABCDEF-1788226930843789",
				WorkspaceDirsRoot: workspaceDirsRoot,
			}

			outputs, _, err := task.RunWorkflowSetup(prov, vars, map[string]*contract.TaskState{}, nil)
			if err != nil {
				t.Fatalf("RunWorkflowSetup: %v", err)
			}
			want := filepath.Join(workspaceDirsRoot, "slack", "C01ABCDEF", "1788226930.843789")
			if got, _ := outputs["workspace_dir"].(string); got != want {
				t.Errorf("workspace_dir = %q, want %q", got, want)
			}
			if info, statErr := os.Stat(want); statErr != nil || !info.IsDir() {
				t.Fatalf("setup did not create %q as a directory: %v", want, statErr)
			}
		})
	}
}
