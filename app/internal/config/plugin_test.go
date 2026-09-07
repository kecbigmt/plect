package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalogsToml(t *testing.T, tmpHome, content string) {
	t.Helper()
	dir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalogs.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_ResolvesEnabledEditableCatalogPlugin(t *testing.T) {
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
`)
	writeFile(t, filepath.Join(catalogDir, "okf", "config", "workspaces", "okf.toml"), `
[okf]
kind = "workspace_provider"

[okf.setup]
type    = "exec"
command = "true"

[okf.health.alive]
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
	if len(cfg.PluginDirs) != 1 {
		t.Fatalf("PluginDirs = %+v, want the resolved plugin dir", cfg.PluginDirs)
	}

	workspaceProviders, err := cfg.LoadWorkspaceProviders()
	if err != nil {
		t.Fatalf("LoadWorkspaceProviders: unexpected error: %v", err)
	}
	if _, ok := workspaceProviders["local.okf.okf"]; !ok {
		t.Fatalf("expected okf workspace provider from the enabled plugin, got %+v", workspaceProviders)
	}
}

func TestLoad_MissingCatalogLockFailsLoud(t *testing.T) {
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
`)

	// A locked (non-editable) path catalog, but plect.lock has no
	// corresponding entry — it was never actually added.
	writeCatalogsToml(t, tmpHome, `
schema_version = 2

[[catalogs]]
alias = "local"
source = "path://`+catalogDir+`"
plugins = ["okf"]
`)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("Load: want an error naming the catalog \"local\", got %v", err)
	}
}

func TestLoad_NoCatalogsRegisteredLeavesPluginDirsAlone(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// No catalogs.toml at all: the absence must not be an error, and
	// existing hand-authored plugin_dirs must be untouched.
	configDir := filepath.Join(tmpHome, ".config", "plect")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("schema_version = 2\nplugin_dirs = [\""+legacyDir+"\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if len(cfg.PluginDirs) != 1 || cfg.PluginDirs[0] != legacyDir {
		t.Fatalf("PluginDirs = %+v, want just the legacy dir %q", cfg.PluginDirs, legacyDir)
	}
}

func TestLoadPlugins_ResolvesEnabledEditableCatalogPlugin(t *testing.T) {
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
`)

	writeCatalogsToml(t, tmpHome, `
schema_version = 2

[[catalogs]]
alias = "local"
source = "path+editable://`+catalogDir+`"
plugins = ["okf"]
`)

	mounted, lock, err := LoadPlugins()
	if err != nil {
		t.Fatalf("LoadPlugins: unexpected error: %v", err)
	}
	if len(mounted) != 1 || mounted[0].ID != "local/okf" {
		t.Fatalf("mounted = %+v, want a single local/okf plugin", mounted)
	}
	if lock == nil {
		t.Fatal("lock = nil, want a non-nil (possibly empty) lockfile")
	}
}

func TestLoadPlugins_NoCatalogsRegisteredReturnsNil(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mounted, lock, err := LoadPlugins()
	if err != nil {
		t.Fatalf("LoadPlugins: unexpected error: %v", err)
	}
	if mounted != nil || lock != nil {
		t.Fatalf("mounted = %+v, lock = %+v, want both nil when no catalogs are registered", mounted, lock)
	}
}
