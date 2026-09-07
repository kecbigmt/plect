package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLoad_UnresolvedCrossPluginBinRefNamesPluginToEnable covers a
// user-authored `bin` reference to an executable of a plugin that exists in
// the catalog but is not enabled: it must fail loud at config load, naming
// the plugin to enable — not wait until that action is actually run
// mid-session. (Shipped plugin config cannot name another plugin at all, so
// the reference has to come from a user-owned layer.)
func TestLoad_UnresolvedCrossPluginBinRefNamesPluginToEnable(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	catalogDir := t.TempDir()
	writeFile(t, filepath.Join(catalogDir, "catalog.toml"), `
schema_version = 2
plugins = ["runtime", "claude"]
`)
	writeFile(t, filepath.Join(catalogDir, "runtime", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"

[[executables]]
name = "activity"
path = "bin/activity"
`)
	writeFile(t, filepath.Join(catalogDir, "claude", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"
`)
	writeFile(t, filepath.Join(tmpHome, ".config", "plect", "config.toml"), "schema_version = 2\n")
	writeFile(t, filepath.Join(tmpHome, ".config", "plect", "tasks", "claude.toml"), `
[claude]
kind  = "effect"
scope = "run"

[claude.setup]
type = "exec"
bin  = "local/runtime/activity"
args = ["claude", "working"]
`)

	writeCatalogsToml(t, tmpHome, `
schema_version = 2

[[catalogs]]
alias = "local"
source = "path+editable://`+catalogDir+`"
plugins = ["claude"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	_, err = cfg.LoadTaskDefinitions("")
	if err == nil {
		t.Fatal("LoadTaskDefinitions: want an error for a bin reference to an unmounted plugin, got nil")
	}
	if !strings.Contains(err.Error(), "local/runtime") {
		t.Errorf("LoadTaskDefinitions error = %v, want it to name the missing plugin \"local/runtime\"", err)
	}
	if !strings.Contains(err.Error(), "plect plugin add local/runtime") {
		t.Errorf("LoadTaskDefinitions error = %v, want a `plect plugin add local/runtime` remediation hint", err)
	}
}

// TestLoad_PluginLocalBinRefStillResolves is a regression guard: a
// bare-name `bin` reference inside a file mounted from its own plugin
// keeps resolving.
func TestLoad_PluginLocalBinRefStillResolves(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	catalogDir := t.TempDir()
	writeFile(t, filepath.Join(catalogDir, "catalog.toml"), `
schema_version = 2
plugins = ["okf"]
`)
	writeFile(t, filepath.Join(catalogDir, "okf", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"

[[executables]]
name = "okf-goal"
path = "bin/okf-goal"
`)
	writeFile(t, filepath.Join(catalogDir, "okf", "config", "tasks", "goal.toml"), `
[goal]
kind = "effect"
scope = "run"

[goal.setup]
type = "exec"
bin  = "okf-goal"
args = ["task", "bootstrap"]

[goal.health.alive]
type = "noop"
`)

	writeCatalogsToml(t, tmpHome, `
schema_version = 2

[[catalogs]]
alias = "local"
source = "path+editable://`+catalogDir+`"
plugins = ["okf"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if _, err := cfg.LoadTaskDefinitions(""); err != nil {
		t.Fatalf("LoadTaskDefinitions: unexpected error for a plugin-local bin reference: %v", err)
	}
}

// TestLoad_UnknownBinRefAliasFailsWithoutFalseRemediation covers a
// reference naming a catalog alias that is not registered at all: the load
// must still fail loud, but must not claim a specific plugin to enable — no
// registered catalog can confirm one exists.
func TestLoad_UnknownBinRefAliasFailsWithoutFalseRemediation(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	catalogDir := t.TempDir()
	writeFile(t, filepath.Join(catalogDir, "catalog.toml"), `
schema_version = 2
plugins = ["claude"]
`)
	writeFile(t, filepath.Join(catalogDir, "claude", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"
`)
	writeFile(t, filepath.Join(catalogDir, "claude", "config", "tasks", "claude.toml"), `
[claude]
kind = "effect"
scope = "run"

[claude.setup]
type = "exec"
bin  = "nonexistent/runtime/activity"
args = ["claude", "working"]
`)

	writeCatalogsToml(t, tmpHome, `
schema_version = 2

[[catalogs]]
alias = "local"
source = "path+editable://`+catalogDir+`"
plugins = ["claude"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	_, err = cfg.LoadTaskDefinitions("")
	if err == nil {
		t.Fatal("LoadTaskDefinitions: want an error for a reference to an unregistered catalog alias, got nil")
	}
	if strings.Contains(err.Error(), "plect plugin add") {
		t.Errorf("LoadTaskDefinitions error = %v, want no `plect plugin add` remediation for an alias no catalog registers", err)
	}
}

// TestLoad_UnresolvedProviderBinRefFailsLoud proves the same load-time check
// covers providers/*.toml (setup/cleanup/subscribe), not just tasks.
func TestLoad_UnresolvedProviderBinRefFailsLoud(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	catalogDir := t.TempDir()
	writeFile(t, filepath.Join(catalogDir, "catalog.toml"), `
schema_version = 2
plugins = ["runtime", "github"]
`)
	writeFile(t, filepath.Join(catalogDir, "runtime", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"

[[executables]]
name = "watcher"
path = "bin/watcher"
`)
	writeFile(t, filepath.Join(catalogDir, "github", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"
`)
	writeFile(t, filepath.Join(catalogDir, "github", "config", "workspaces", "github.toml"), `
[github]
kind = "workspace_provider"

[github.setup]
type    = "exec"
command = "true"

[github.health.alive]
type = "noop"

[github.subscribe]
type = "exec"
bin  = "local/runtime/watcher"
`)

	writeCatalogsToml(t, tmpHome, `
schema_version = 2

[[catalogs]]
alias = "local"
source = "path+editable://`+catalogDir+`"
plugins = ["github"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	_, err = cfg.LoadWorkspaceProviders()
	if err == nil {
		t.Fatal("LoadWorkspaceProviders: want an error for a bin reference to an unmounted plugin, got nil")
	}
	if !strings.Contains(err.Error(), "local/runtime") {
		t.Errorf("LoadWorkspaceProviders error = %v, want it to name the missing plugin \"local/runtime\"", err)
	}
}

// TestLoad_UnresolvedResourceBinRefFailsLoud proves the same load-time check
// covers a resource observer's actions. The reference is user-owned here
// because shipped plugin config may not name another plugin's executable at
// all — see TestLoadResourceDefs_PluginOwnedBinRefStaysInsideItsPlugin.
func TestLoad_UnresolvedResourceBinRefFailsLoud(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	catalogDir := t.TempDir()
	writeFile(t, filepath.Join(catalogDir, "catalog.toml"), `
schema_version = 2
plugins = ["runtime", "github"]
`)
	writeFile(t, filepath.Join(catalogDir, "runtime", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"

[[executables]]
name = "watcher"
path = "bin/watcher"
`)
	writeFile(t, filepath.Join(catalogDir, "github", "plugin.toml"), `
schema_version = 2
plect_min_version = "0.0.0"
`)
	writeFile(t, filepath.Join(tmpHome, ".config", "plect", "config.toml"), "schema_version = 2\n")
	writeFile(t, filepath.Join(tmpHome, ".config", "plect", "resources", "github.toml"), `
[github]
kind  = "resource_observer"
match = "^gh:"

[github.observe]
type = "exec"
bin  = "local/runtime/watcher"
`)

	writeCatalogsToml(t, tmpHome, `
schema_version = 2

[[catalogs]]
alias = "local"
source = "path+editable://`+catalogDir+`"
plugins = ["github"]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	_, err = cfg.LoadResourceDefs()
	if err == nil {
		t.Fatal("LoadResourceDefs: want an error for a bin reference to an unmounted plugin, got nil")
	}
	if !strings.Contains(err.Error(), "local/runtime") {
		t.Errorf("LoadResourceDefs error = %v, want it to name the missing plugin \"local/runtime\"", err)
	}
}
