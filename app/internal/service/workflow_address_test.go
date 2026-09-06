package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// writePluginWorkflow stands up one catalog plugin that declares a whole
// workflow — its effect and workspace provider included, so every reference
// inside it is relative and resolves in the plugin's own namespace.
func writePluginWorkflow(t *testing.T, wfID, marker string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(dir, "config", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("tasks", "noop.toml"), `
[noop]
kind  = "effect"
scope = "session"

[noop.setup]
type   = "shell"
script = "echo '{}'"

[noop.health.alive]
type = "noop"
`)
	write(filepath.Join("workspaces", "space.toml"), `
[space]
kind  = "workspace_provider"
match = '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(issues|pull)/(?P<number>\d+)'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[space.setup]
type   = "shell"
script = "mkdir -p `+marker+` && printf '{\"workspace_dir\":\"`+marker+`\"}'"
`)
	write(filepath.Join("workflows", wfID+".toml"), `
[`+wfID+`]
kind               = "workflow"
workspace_provider = "space"

[[`+wfID+`.nodes]]
uses = "noop"
`)
	return dir
}

// A session freezes the address its workflow answers to. Freezing the bare id
// would leave the next command looking up a name no map holds — which the
// idempotent second Create exercises directly, since it compares the frozen
// value against the workflow it just resolved.
func TestCreate_PluginOwnedWorkflowFreezesItsAddress(t *testing.T) {
	store := testStore(t)
	dir := writePluginWorkflow(t, "shared", filepath.Join(t.TempDir(), "wd"))
	cfg := &config.Config{
		BaseDir:    t.TempDir(),
		PluginDirs: []string{dir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: dir}},
	}

	url := "https://github.com/org/repo/issues/42"
	result, err := Create(cfg, store, CreateParams{URL: url, Workflow: "official.acme.shared"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s := store.Get(result.SessionName)
	if s == nil {
		t.Fatalf("no session recorded for %q", result.SessionName)
	}
	if s.Workflow != "official.acme.shared" {
		t.Errorf("frozen workflow = %q, want the address it was selected by", s.Workflow)
	}
	// The plan has to reload against the frozen value.
	if _, err := Create(cfg, store, CreateParams{URL: url, Workflow: "official.acme.shared"}); err != nil {
		t.Errorf("second Create: %v", err)
	}
}

// The acceptance case: two plugins declaring one workflow id both run. Their
// session names still need distinct tags, because a workflow id is the default
// session tag and that is naming rather than addressing.
func TestCreate_TwoPluginsSharingAWorkflowIDRunSideBySide(t *testing.T) {
	store := testStore(t)
	dirA := writePluginWorkflow(t, "shared", filepath.Join(t.TempDir(), "a"))
	dirB := writePluginWorkflow(t, "shared", filepath.Join(t.TempDir(), "b"))
	cfg := &config.Config{
		BaseDir:    t.TempDir(),
		PluginDirs: []string{dirA, dirB},
		Plugins: []plugins.Mounted{
			{ID: "official/a", Dir: dirA},
			{ID: "official/b", Dir: dirB},
		},
	}

	url := "https://github.com/org/repo/issues/42"
	for _, tc := range []struct{ address, tag string }{
		{"official.a.shared", "a"},
		{"official.b.shared", "b"},
	} {
		result, err := Create(cfg, store, CreateParams{URL: url, Workflow: tc.address, Tag: tc.tag})
		if err != nil {
			t.Fatalf("Create(%s): %v", tc.address, err)
		}
		s := store.Get(result.SessionName)
		if s == nil || s.Workflow != tc.address {
			t.Fatalf("session %q froze %q, want %q", result.SessionName, s.Workflow, tc.address)
		}
	}
}

// A dynamic instance's stored address is authoritative even when it equals the
// instance key. A `--name` chosen to match a workflow node's id must not make
// the node's declaration answer for the instance.
func TestTaskCleanup_NamedDynamicInstanceKeepsItsOwnDeclaration(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "user-ran")
	pluginDir, base := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A plugin effect and a user effect share the id "runner"; the workflow's
	// node runs the plugin's.
	write(filepath.Join(pluginDir, "config", "tasks", "runner.toml"), `
[runner]
kind  = "effect"
scope = "session"

[runner.setup]
type   = "shell"
script = "echo '{}'"

[runner.health.alive]
type = "noop"

[runner.cleanup]
type   = "shell"
script = "exit 3"
`)
	write(filepath.Join(base, "tasks", "runner.toml"), `
[runner]
kind  = "effect"
scope = "session"

[runner.setup]
type   = "shell"
script = "echo '{}'"

[runner.health.alive]
type = "noop"

[runner.cleanup]
type   = "shell"
script = "touch `+marker+`"
`)
	write(filepath.Join(base, "workflows", "coding.toml"), `
[coding]
kind = "workflow"

[[coding.nodes]]
id   = "runner"
uses = "official.acme.runner"
`)
	cfg := &config.Config{
		BaseDir:    base,
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	// A dynamic instance of the USER's effect, named to collide with the node.
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "runner", SessionName: "o/r-1", Name: "runner"}); err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "runner", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("TaskCleanup: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the user-owned effect's cleanup did not run, so the node's declaration answered for a dynamic instance: %v", err)
	}
}

// A named dynamic instance may share its key with a workflow node that was
// never instantiated: `--name` collides only against existing state, and an
// uninstantiated node has none. Teardown must tear down what the state entry
// says it is — the user's dynamic instance — rather than deciding by name that
// the plan's node owns the key and running that effect's cleanup.
func TestDestroy_NamedDynamicInstanceCollidingWithAnUninstantiatedNode(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "user-ran")
	pluginDir, base := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pluginDir, "config", "tasks", "runner.toml"), `
[runner]
kind  = "effect"
scope = "session"

[runner.setup]
type   = "shell"
script = "echo '{}'"

[runner.health.alive]
type = "noop"

[runner.cleanup]
type   = "shell"
script = "exit 3"
`)
	write(filepath.Join(base, "tasks", "runner.toml"), `
[runner]
kind  = "effect"
scope = "session"

[runner.setup]
type   = "shell"
script = "echo '{}'"

[runner.health.alive]
type = "noop"

[runner.cleanup]
type   = "shell"
script = "touch `+marker+`"
`)
	write(filepath.Join(base, "workflows", "coding.toml"), `
[coding]
kind = "workflow"

[[coding.nodes]]
id   = "runner"
uses = "official.acme.runner"
`)
	cfg := &config.Config{
		BaseDir:    base,
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	// The node is never set up; the name is therefore free.
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "runner", SessionName: "o/r-1", Name: "runner"}); err != nil {
		t.Fatalf("TaskSetup: %v", err)
	}
	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "o/r-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the dynamic instance's own cleanup did not run: %v", err)
	}
}

// Ambiguity has to name something the reader can pass to --workflow. Two
// plugins declaring one id would otherwise print that id twice.
func TestDispatch_AmbiguityNamesAddressesNotIds(t *testing.T) {
	dirA := writePluginWorkflow(t, "shared", filepath.Join(t.TempDir(), "a"))
	dirB := writePluginWorkflow(t, "shared", filepath.Join(t.TempDir(), "b"))
	cfg := &config.Config{
		BaseDir:    t.TempDir(),
		PluginDirs: []string{dirA, dirB},
		Plugins: []plugins.Mounted{
			{ID: "official/a", Dir: dirA},
			{ID: "official/b", Dir: dirB},
		},
	}

	_, _, err := dispatchResource(cfg, "", "https://github.com/org/repo/issues/42")
	if err == nil {
		t.Fatal("expected two auto-selecting workflows to be ambiguous")
	}
	for _, want := range []string{"official.a.shared", "official.b.shared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}
