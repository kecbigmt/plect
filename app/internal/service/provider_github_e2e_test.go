//go:build integration

package service

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// shippedGithubWorkspaceProvider loads the workspace provider config that
// ships with the GitHub catalog plugin, so the invariant/convergence tests
// exercise the hooks that ship rather than a fixture. It also builds the
// binaries those hooks resolve through `{{bin ...}}` and returns the
// mounted-plugin entry that resolution needs, since the hooks are what is
// under test here.
//
// The workspace provider is loaded from inside the same directory
// buildWorkspaceProviderBinaries mounted (not straight from the repo's
// plugins/github/), because plugin-local `{{bin "<name>"}}` resolution finds
// the containing plugin from the workspace provider's own SourcePath — it
// must actually live under the mounted plugin's directory, the way a real
// catalog mount always does. The copy is made here, not inside
// buildWorkspaceProviderBinaries, because attachGithubWorkspaceProvider (the
// other buildWorkspaceProviderBinaries caller) writes its own
// differently-named workspace provider file into the same mounted
// directory — a second, unconditional "github.toml" there would collide
// with it under a repeated-alias test like
// TestIntegration_WorkflowFrozenOnSession's two-workflow fixture.
func shippedGithubWorkspaceProvider(t *testing.T) (config.WorkspaceProviderConfig, []plugins.Mounted) {
	t.Helper()
	root := repoRoot(t)
	mounted := buildWorkspaceProviderBinaries(t, root)
	workspacesDir := filepath.Join(mounted[0].Dir, "config", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join(root, "plugins", "github", "config", "workspaces", "worktree.toml"))
	if err != nil {
		t.Fatalf("read shipped github workspace provider: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacesDir, "github.toml"), shipped, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{PluginDirs: []string{mounted[0].Dir}, Plugins: mounted}
	provs, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("load mounted github workspace provider: %v", err)
	}
	prov, ok := provs["official.github.worktree"]
	if !ok {
		t.Fatal(`mounted github workspace provider: "official.github.worktree" not found`)
	}
	return prov, mounted
}

// goToolCaches holds the module and build cache locations resolved before any
// test rewrites HOME. Without them, a build issued from a test whose HOME is a
// temp directory would populate a throwaway module cache on every run.
var goToolCaches = resolveGoToolCaches()

func resolveGoToolCaches() []string {
	out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return nil
	}
	return []string{"GOMODCACHE=" + lines[0], "GOCACHE=" + lines[1]}
}

var workspaceProviderBinaries = []struct{ moduleDir, pkg, name string }{
	{"app", "./cmd/plect", "plect"},
	{filepath.Join("plugins", "github", "src"), "./cmd/github-worktree", "github-worktree"},
	{filepath.Join("plugins", "github", "src"), "./cmd/github-watcher", "github-watcher"},
	{filepath.Join("plugins", "github", "src"), "./cmd/gh-app-token", "gh-app-token"},
}

// buildSharedWorkspaceProviderBinaries builds workspaceProviderBinaries once
// per test binary run: dozens of tests each mount a workspace-provider
// fixture, and a per-test rebuild of all four was this package's dominant
// integration-test cost.
func buildSharedWorkspaceProviderBinaries(root string) (string, error) {
	sharedWorkspaceProviderBinariesOnce.Do(func() {
		dir, err := os.MkdirTemp("", "plect-workspace-provider-bin-")
		if err != nil {
			sharedWorkspaceProviderBinariesErr = err
			return
		}
		for _, b := range workspaceProviderBinaries {
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, b.name), b.pkg)
			cmd.Dir = filepath.Join(root, b.moduleDir)
			cmd.Env = append(os.Environ(), goToolCaches...)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				sharedWorkspaceProviderBinariesErr = fmt.Errorf("build %s: %w", b.name, err)
				return
			}
		}
		sharedWorkspaceProviderBinariesDir = dir
	})
	return sharedWorkspaceProviderBinariesDir, sharedWorkspaceProviderBinariesErr
}

// buildWorkspaceProviderBinaries symlinks the shared binaries (see
// buildSharedWorkspaceProviderBinaries) into a fresh per-test directory and
// PATH, so a test's own config/workspaces writes don't collide with
// another test's.
func buildWorkspaceProviderBinaries(t *testing.T, root string) []plugins.Mounted {
	t.Helper()
	sharedDir, err := buildSharedWorkspaceProviderBinaries(root)
	if err != nil {
		t.Fatalf("build workspace provider binaries: %v", err)
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range workspaceProviderBinaries {
		if err := os.Symlink(filepath.Join(sharedDir, b.name), filepath.Join(binDir, b.name)); err != nil {
			t.Fatalf("symlink %s: %v", b.name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return []plugins.Mounted{{
		ID:  "official/github",
		Dir: binDir,
		Manifest: plugins.Manifest{Executables: []plugins.Executable{
			{Name: "github-worktree", Path: "github-worktree"},
			{Name: "github-watcher", Path: "github-watcher"},
			{Name: "gh-app-token", Path: "gh-app-token"},
		}},
	}}
}

func TestBuildWorkspaceProviderBinaries_BuildsOnce(t *testing.T) {
	root := repoRoot(t)
	buildWorkspaceProviderBinaries(t, root)
	firstDir, err := buildSharedWorkspaceProviderBinaries(root)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(filepath.Join(firstDir, "plect"))
	if err != nil {
		t.Fatal(err)
	}

	buildWorkspaceProviderBinaries(t, root)
	secondDir, err := buildSharedWorkspaceProviderBinaries(root)
	if err != nil {
		t.Fatal(err)
	}
	if secondDir != firstDir {
		t.Fatalf("shared binaries directory changed between calls: %q vs %q", firstDir, secondDir)
	}
	secondInfo, err := os.Stat(filepath.Join(secondDir, "plect"))
	if err != nil {
		t.Fatal(err)
	}
	if !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Error("plect was rebuilt on a second mount request; workspace-provider binaries must build exactly once per test binary run")
	}
}

// setupHomeRepo builds the bare-ish layout the github workspace provider
// expects under $HOME/workdirs (the path the setup script hard-codes) and
// points HOME at it.
func setupHomeRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	gitDir := filepath.Join(home, "workspace_dirs", "github.com", "testowner", "testrepo", "main")
	bareDir := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("setup %v: %v", args, err)
		}
	}
	run(t.TempDir(), "git", "init", "--bare", "-b", "main", bareDir)
	run(gitDir, "git", "init", "-b", "main")
	run(gitDir, "git", "config", "user.email", "test@test.com")
	run(gitDir, "git", "config", "user.name", "Test")
	run(gitDir, "git", "commit", "--allow-empty", "-m", "init")
	run(gitDir, "git", "remote", "add", "origin", bareDir)
	run(gitDir, "git", "push", "-u", "origin", "main")
	return gitDir
}

// setupSrcRepo builds the ~/src layout: a primary checkout plus a workdir
// container that is deliberately NOT a repository. Returns the primary
// checkout, which is the git dir both setup and cleanup must resolve to.
func setupSrcRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	srcDir := filepath.Join(home, "src", "github.com", "testowner", "testrepo")
	container := filepath.Join(home, "workspace_dirs", "github.com", "testowner", "testrepo")
	bareDir := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("setup %v: %v", args, err)
		}
	}
	run(t.TempDir(), "git", "init", "--bare", "-b", "main", bareDir)
	run(srcDir, "git", "init", "-b", "main")
	run(srcDir, "git", "config", "user.email", "test@test.com")
	run(srcDir, "git", "config", "user.name", "Test")
	run(srcDir, "git", "commit", "--allow-empty", "-m", "init")
	run(srcDir, "git", "remote", "add", "origin", bareDir)
	run(srcDir, "git", "push", "-u", "origin", "main")
	return srcDir
}

// watcherSubscriptions reads the real github-watcher's on-disk subscription
// registry (~/.local/share/github-watcher/subscriptions.json, following its
// own XDG default) directly: core never imports the plugin's watcher
// package, so this is the only way an app-side e2e test can see what the
// real `subscribe`/`unsubscribe` hooks — the shipped worktree.toml this
// package cannot otherwise observe — actually persisted.
func watcherSubscriptions(t *testing.T, home string) map[string]json.RawMessage {
	t.Helper()
	path := filepath.Join(home, ".local", "share", "github-watcher", "subscriptions.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read subscriptions.json: %v", err)
	}
	var doc struct {
		Subscriptions map[string]json.RawMessage `json:"subscriptions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse subscriptions.json: %v", err)
	}
	return doc.Subscriptions
}

// TestE2E_GithubWorkspaceProviderWiresAndDropsEventDelivery pins
// binding-implies-delivery against the real shipped hooks, not a fixture
// provider: binding a resource via subscribeIfWired — the same call
// TaskSetup makes for an explicit --resource — must actually register a
// subscription the real github-watcher binary persists, through the shipped
// worktree.toml's `subscribe` hook. unsubscribeIfWired — TaskCleanup's
// counterpart — must drop exactly that one registration through the
// worktree.toml's `unsubscribe` hook.
func TestE2E_GithubWorkspaceProviderWiresAndDropsEventDelivery(t *testing.T) {
	root := repoRoot(t)
	mounted := buildWorkspaceProviderBinaries(t, root)
	workspacesDir := filepath.Join(mounted[0].Dir, "config", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join(root, "plugins", "github", "config", "workspaces", "worktree.toml"))
	if err != nil {
		t.Fatalf("read shipped github workspace provider: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacesDir, "github.toml"), shipped, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{PluginDirs: []string{mounted[0].Dir}, Plugins: mounted}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	const session = "testowner/testrepo-42+review"
	const resource = "https://github.com/testowner/testrepo/pull/42"

	wired, err := subscribeIfWired(cfg, session, resource, "")
	if err != nil {
		t.Fatalf("subscribeIfWired: %v", err)
	}
	if !wired {
		t.Fatal("the shipped github workspace provider must wire delivery for an issue/PR URL")
	}
	subs := watcherSubscriptions(t, home)
	if len(subs) != 1 {
		t.Fatalf("subscriptions after subscribeIfWired = %v, want exactly one", subs)
	}

	unsubscribed, err := unsubscribeIfWired(cfg, session, resource)
	if err != nil {
		t.Fatalf("unsubscribeIfWired: %v", err)
	}
	if !unsubscribed {
		t.Fatal("the shipped github workspace provider must unwire delivery for an issue/PR URL")
	}
	subs = watcherSubscriptions(t, home)
	if len(subs) != 0 {
		t.Fatalf("subscriptions after unsubscribeIfWired = %v, want none", subs)
	}
}

// TestE2E_GithubWorkspaceProviderSubscribeForwardsBranch pins the issue this
// package's subscribeIfWired call cannot see on its own: an issue session's
// branch must reach the real github-watcher's persisted subscription through
// the shipped worktree.toml's `subscribe` hook, since the watcher's own
// linked-PR discovery for an issue resource reads it from there.
func TestE2E_GithubWorkspaceProviderSubscribeForwardsBranch(t *testing.T) {
	root := repoRoot(t)
	mounted := buildWorkspaceProviderBinaries(t, root)
	workspacesDir := filepath.Join(mounted[0].Dir, "config", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shipped, err := os.ReadFile(filepath.Join(root, "plugins", "github", "config", "workspaces", "worktree.toml"))
	if err != nil {
		t.Fatalf("read shipped github workspace provider: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacesDir, "github.toml"), shipped, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{PluginDirs: []string{mounted[0].Dir}, Plugins: mounted}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")

	const session = "testowner/testrepo-42+review"
	const resource = "https://github.com/testowner/testrepo/issues/42"
	const branch = "issue/42+review"

	if _, err := subscribeIfWired(cfg, session, resource, branch); err != nil {
		t.Fatalf("subscribeIfWired: %v", err)
	}
	subs := watcherSubscriptions(t, home)
	if len(subs) != 1 {
		t.Fatalf("subscriptions after subscribeIfWired = %v, want exactly one", subs)
	}
	for _, raw := range subs {
		var sub struct {
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal(raw, &sub); err != nil {
			t.Fatalf("parse persisted subscription: %v", err)
		}
		if sub.Branch != branch {
			t.Errorf("persisted subscription branch = %q, want %q", sub.Branch, branch)
		}
	}
}

// TestIntegration_CreateSubscribesToGithubWatcher exercises Create itself
// against the real shipped github-watcher hooks, not subscribeIfWired
// directly as above.
func TestIntegration_CreateSubscribesToGithubWatcher(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	home := os.Getenv("HOME")
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{{id: "noop", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "noop"}},
	)

	url := "https://github.com/testowner/testrepo/issues/55"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	subs := watcherSubscriptions(t, home)
	if len(subs) != 1 {
		t.Fatalf("subscriptions after Create = %v, want exactly one", subs)
	}
}

// TestIntegration_DestroyUnsubscribesFromGithubWatcher is the previous
// test's teardown counterpart, against the real shipped hooks.
func TestIntegration_DestroyUnsubscribesFromGithubWatcher(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	home := os.Getenv("HOME")
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{{id: "noop", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "noop"}},
	)

	url := "https://github.com/testowner/testrepo/issues/56"
	sessionName := "testowner/testrepo-56+default"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if subs := watcherSubscriptions(t, home); len(subs) != 1 {
		t.Fatalf("precondition: subscriptions after Create = %v, want exactly one", subs)
	}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName, Force: true}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if subs := watcherSubscriptions(t, home); len(subs) != 0 {
		t.Fatalf("subscriptions after Destroy = %v, want none", subs)
	}
}

// TestE2E_GithubWorkspaceProviderSrcLayoutSingleWorkdir covers the ~/src
// layout's thinnest case: the session owns the container's only workdir.
// Cleanup has no sibling workdir and no bare layout to fall back to, so
// resolving the primary checkout is the only thing keeping destroy from
// stranding it.
func TestE2E_GithubWorkspaceProviderSrcLayoutSingleWorkdir(t *testing.T) {
	setupFakeScripts(t)
	setupSrcRepo(t)
	prov, mounted := shippedGithubWorkspaceProvider(t)
	session := "testowner/testrepo-42+review"

	tasks := map[string]*contract.TaskState{}
	vars := effect.WorkflowHookVars{ResourceID: "https://github.com/testowner/testrepo/issues/42", SessionName: session, Plugins: mounted, SourcePath: prov.SourcePath}
	out, err := task.RunWorkflowSetup(prov, vars, tasks, nil)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	workdir, _ := out["workspace_dir"].(string)
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("workdir not created: %v", err)
	}
	if !strings.Contains(workdir, filepath.Join("workspace_dirs", "github.com", "testowner", "testrepo")) {
		t.Errorf("workdir = %q, want it inside the workdir container", workdir)
	}

	if err := task.RunWorkflowCleanup(prov, vars, tasks, nil); err != nil {
		t.Fatalf("cleanup on the container's only workdir: %v", err)
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Errorf("workdir should be removed, stat err: %v", err)
	}
}

func runSetup(t *testing.T, prov config.WorkspaceProviderConfig, mounted []plugins.Mounted, session string) map[string]any {
	t.Helper()
	tasks := map[string]*contract.TaskState{}
	out, err := task.RunWorkflowSetup(prov, effect.WorkflowHookVars{
		ResourceID:  "https://github.com/testowner/testrepo/issues/42",
		SessionName: session,
		Plugins:     mounted,
		SourcePath:  prov.SourcePath,
	}, tasks, nil)
	if err != nil {
		t.Fatalf("provider setup: %v", err)
	}
	return out
}

// TestE2E_GithubWorkspaceProviderInvariant runs the shipped github setup and
// asserts the session is a function of the tagged session name (branch +
// workdir path both carry the tag).
func TestE2E_GithubWorkspaceProviderInvariant(t *testing.T) {
	setupFakeScripts(t)
	setupHomeRepo(t)
	prov, mounted := shippedGithubWorkspaceProvider(t)

	out := runSetup(t, prov, mounted, "testowner/testrepo-42+review")

	if got := out["branch"]; got != "issue/42+review" {
		t.Errorf("branch = %v, want issue/42+review (derived from tagged session name)", got)
	}
	workdir, _ := out["workspace_dir"].(string)
	if !strings.HasSuffix(workdir, "/issue-42-review") {
		t.Errorf("workdir = %q, want suffix /issue-42-review", workdir)
	}
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("workdir not created: %v", err)
	}
	// A different tool's session on the same resource must land in a distinct
	// session (the cross-tool separation the default tag guarantees).
	out2 := runSetup(t, prov, mounted, "testowner/testrepo-42+claude")
	if out2["workspace_dir"] == workdir {
		t.Error("tagged sessions must not share a session")
	}
}

// TestE2E_GithubWorkspaceProviderConvergesAndReclaims covers destroy
// convergence on the live workspace provider path: cleanup reclaims the
// tagged branch, and a re-dispatch over an orphaned branch reuses it
// instead of failing.
func TestE2E_GithubWorkspaceProviderConvergesAndReclaims(t *testing.T) {
	setupFakeScripts(t)
	gitDir := setupHomeRepo(t)
	prov, mounted := shippedGithubWorkspaceProvider(t)
	session := "testowner/testrepo-42+review"
	branch := "issue/42+review"

	// First dispatch.
	tasks := map[string]*contract.TaskState{}
	// delete_branch opts into branch reclaim on cleanup — the default is now
	// to leave it, so this test's own name ("...AndReclaims") requires it.
	vars := effect.WorkflowHookVars{
		ResourceID:    "https://github.com/testowner/testrepo/issues/42",
		SessionName:   session,
		Plugins:       mounted,
		SourcePath:    prov.SourcePath,
		CleanupInputs: map[string]string{"delete_branch": "true"},
	}
	if _, err := task.RunWorkflowSetup(prov, vars, tasks, nil); err != nil {
		t.Fatalf("setup: %v", err)
	}

	branchExists := func() bool {
		cmd := exec.Command("git", "-C", gitDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
		return cmd.Run() == nil
	}
	if !branchExists() {
		t.Fatal("precondition: tagged branch should exist after setup")
	}

	// Cleanup reclaims workdir + branch (branch is merged into main → safe -d).
	if err := task.RunWorkflowCleanup(prov, vars, tasks, nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	workdir, _ := tasks[contract.WorkflowPseudoNodeID].Outputs["workspace_dir"].(string)
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Errorf("workdir should be removed, stat err: %v", err)
	}
	if branchExists() {
		t.Error("cleanup must reclaim the tagged branch")
	}

	// Now simulate an orphan: re-create the branch + workdir, then remove only
	// the workdir (branch survives), and re-dispatch. Setup must converge.
	tasks2 := map[string]*contract.TaskState{}
	out := runSetup(t, prov, mounted, session)
	wd, _ := out["workspace_dir"].(string)
	// "worktree", not "workspace_dir": this is the literal git subcommand, not
	// plect's own vocabulary for the acquired directory.
	if err := exec.Command("git", "-C", gitDir, "worktree", "remove", wd).Run(); err != nil {
		t.Fatalf("orphan the branch (remove workdir): %v", err)
	}
	if !branchExists() {
		t.Fatal("precondition: branch should survive workdir removal")
	}
	tasks2 = map[string]*contract.TaskState{}
	if _, err := task.RunWorkflowSetup(prov, vars, tasks2, nil); err != nil {
		t.Fatalf("re-dispatch over orphan branch should reuse it, got: %v", err)
	}
}
