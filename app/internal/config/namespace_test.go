package config

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/plugins"
)

// twoAddressablePlugins mounts two catalog-qualified plugins, each carrying
// one declaration file, and returns a Config that reads both.
func twoAddressablePlugins(t *testing.T, relPath, bodyA, bodyB string) *Config {
	t.Helper()
	dirA, dirB := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dirA, "config", relPath), bodyA)
	writeFile(t, filepath.Join(dirB, "config", relPath), bodyB)
	return &Config{
		PluginDirs: []string{dirA, dirB},
		Plugins: []plugins.Mounted{
			{ID: "official/a", Dir: dirA},
			{ID: "official/b", Dir: dirB},
		},
	}
}

func TestLoadResourceDefs_TwoAddressablePluginsShareAnID(t *testing.T) {
	observer := func(match string) string {
		return `
[github]
kind  = "resource_observer"
match = '` + match + `'

[github.observe]
type    = "exec"
command = "true"
`
	}
	cfg := twoAddressablePlugins(t, filepath.Join("resources", "github.toml"), observer("^a"), observer("^b"))

	got, err := cfg.LoadResourceDefs()
	if err != nil {
		t.Fatalf("LoadResourceDefs: %v", err)
	}
	if _, ok := got["official.a.github"]; !ok {
		t.Errorf("official.a.github missing; keys = %v", sortedKeysOf(got))
	}
	if _, ok := got["official.b.github"]; !ok {
		t.Errorf("official.b.github missing; keys = %v", sortedKeysOf(got))
	}
	if _, ok := got["github"]; ok {
		t.Errorf("a plugin-owned declaration is addressed by its catalog address, not the bare id; keys = %v", sortedKeysOf(got))
	}
}

func TestLoadChannels_TwoAddressablePluginsShareAnID(t *testing.T) {
	channel := func(cmd string) string {
		return "[delivery]\nkind = \"channel\"\ntype = \"exec\"\ncommand = \"" + cmd + "\"\n"
	}
	cfg := twoAddressablePlugins(t, filepath.Join("channels", "delivery.toml"), channel("a"), channel("b"))

	got, err := cfg.LoadChannels()
	if err != nil {
		t.Fatalf("LoadChannels: %v", err)
	}
	if _, ok := got["official.a.delivery"]; !ok {
		t.Errorf("official.a.delivery missing; keys = %v", sortedKeysOf(got))
	}
	if _, ok := got["official.b.delivery"]; !ok {
		t.Errorf("official.b.delivery missing; keys = %v", sortedKeysOf(got))
	}
}

func TestLoadWorkspaceProviders_TwoAddressablePluginsShareAnID(t *testing.T) {
	provider := func(cmd string) string {
		return `
[worktree]
kind = "workspace_provider"

[worktree.setup]
type    = "exec"
command = "` + cmd + `"
`
	}
	cfg := twoAddressablePlugins(t, filepath.Join("workspaces", "worktree.toml"), provider("a"), provider("b"))

	got, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("LoadWorkspaceProviders: %v", err)
	}
	if got["official.a.worktree"].Setup.Command != "a" || got["official.b.worktree"].Setup.Command != "b" {
		t.Errorf("both plugins' worktree providers should coexist; keys = %v", sortedKeysOf(got))
	}
}

func TestLoadTaskDefinitions_TwoAddressablePluginsShareAnID(t *testing.T) {
	effect := func(script string) string {
		return `
[runtime]
kind  = "effect"
scope = "run"

[runtime.setup]
type   = "shell"
script = "echo ` + script + `"

[runtime.health.alive]
type = "noop"
`
	}
	cfg := twoAddressablePlugins(t, filepath.Join("tasks", "runtime.toml"), effect("a"), effect("b"))

	got, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("LoadTaskDefinitions: %v", err)
	}
	if _, ok := got["official.a.runtime"]; !ok {
		t.Errorf("official.a.runtime missing; keys = %v", sortedKeysOf(got))
	}
	if _, ok := got["official.b.runtime"]; !ok {
		t.Errorf("official.b.runtime missing; keys = %v", sortedKeysOf(got))
	}
}

func TestLoadWorkflows_TwoAddressablePluginsShareAnID(t *testing.T) {
	workflow := func(uses string) string {
		return `
[shared]
kind = "workflow"

[[shared.nodes]]
uses = "` + uses + `"
`
	}
	cfg := twoAddressablePlugins(t, filepath.Join("workflows", "shared.toml"), workflow("a"), workflow("b"))

	got, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatalf("LoadWorkflows: %v", err)
	}
	if _, ok := got["official.a.shared"]; !ok {
		t.Errorf("official.a.shared missing; keys = %v", sortedKeysOf(got))
	}
	if _, ok := got["official.b.shared"]; !ok {
		t.Errorf("official.b.shared missing; keys = %v", sortedKeysOf(got))
	}
}

// A user-owned declaration is addressed by its bare id: the user-owned layer
// stack is one namespace and has no alias to qualify with.
func TestLoadResourceDefs_UserOwnedDeclarationKeepsItsBareID(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "resources", "github.toml"), `
[github]
kind  = "resource_observer"
match = '^x'

[github.observe]
type    = "exec"
command = "true"
`)
	got, err := (&Config{BaseDir: base}).LoadResourceDefs()
	if err != nil {
		t.Fatalf("LoadResourceDefs: %v", err)
	}
	if _, ok := got["github"]; !ok {
		t.Errorf("github missing; keys = %v", sortedKeysOf(got))
	}
}

func sortedKeysOf[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
