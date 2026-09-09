package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/version"
)

// resolveDeclaredPlugins prepends cfg.PluginDirs with the mount directories
// of every plugin enabled across registered catalogs in
// ~/.config/plect/catalogs.toml, resolved purely from local state
// (plugins.VerifyAndMountAll never fetches over the network, so this runs
// on every config load). A missing catalogs.toml is not an error — it means
// the user has registered no catalogs. Any resolution failure (an
// unregistered catalog, a missing or mismatched lock entry, tampered
// content, or a plect_min_version violation) fails the whole load: per the
// plugin-packaging design, a plugin problem must never silently mount
// nothing and continue.
// LoadPlugins resolves every plugin enabled across registered catalogs,
// purely from local state — the same resolution resolveDeclaredPlugins
// applies inside Load(). It is kept as an independent function rather than
// a shared helper because its caller (the plugin service supervisor, polling
// for plugin content changes) only needs the mounted plugin list and
// lockfile, not a full Config with workspaces/resources/tasks/workflows/
// channels loaded on every poll tick. A missing catalogs.toml or no
// registered catalogs returns (nil, nil, nil): not an error, just nothing
// declared.
func LoadPlugins() ([]plugins.Mounted, *plugins.Lockfile, error) {
	catalogsPath, err := plugins.DefaultCatalogsPath()
	if err != nil {
		return nil, nil, nil
	}
	registrations, err := plugins.LoadCatalogRegistrations(catalogsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load catalog registrations: %w", err)
	}
	if len(registrations.Catalogs) == 0 {
		return nil, nil, nil
	}

	lockPath, err := plugins.DefaultLockfilePath()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve plect.lock path: %w", err)
	}
	lock, err := plugins.LoadLockfile(lockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load plect.lock: %w", err)
	}
	cacheRoot, err := plugins.DefaultCacheRoot()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve catalog cache root: %w", err)
	}

	mounted, err := plugins.VerifyAndMountAll(registrations, lock, cacheRoot, version.Current)
	if err != nil {
		return nil, nil, err
	}
	return mounted, lock, nil
}

func resolveDeclaredPlugins(cfg *Config) (*Config, error) {
	catalogsPath, err := plugins.DefaultCatalogsPath()
	if err != nil {
		return cfg, nil
	}
	registrations, err := plugins.LoadCatalogRegistrations(catalogsPath)
	if err != nil {
		return nil, fmt.Errorf("load catalog registrations: %w", err)
	}
	if len(registrations.Catalogs) == 0 {
		return cfg, nil
	}

	lockPath, err := plugins.DefaultLockfilePath()
	if err != nil {
		return nil, fmt.Errorf("resolve plect.lock path: %w", err)
	}
	lock, err := plugins.LoadLockfile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("load plect.lock: %w", err)
	}
	cacheRoot, err := plugins.DefaultCacheRoot()
	if err != nil {
		return nil, fmt.Errorf("resolve catalog cache root: %w", err)
	}

	mounted, err := plugins.VerifyAndMountAll(registrations, lock, cacheRoot, version.Current)
	if err != nil {
		return nil, err
	}
	resolvedDirs := make([]string, len(mounted))
	for i, m := range mounted {
		resolvedDirs[i] = m.Dir
	}
	cfg.PluginDirs = append(resolvedDirs, cfg.PluginDirs...)
	cfg.Plugins = mounted
	cfg.catalogRegistrations = registrations
	cfg.catalogLock = lock
	cfg.catalogCacheRoot = cacheRoot
	return cfg, nil
}

// pluginLayerOf names the plugin layer that mounted sourcePath, and is empty
// for a user-owned layer. A catalog-mounted plugin is named by its
// catalog-qualified id, which is what a user-owned reference addresses it by;
// a hand-authored `plugin_dirs` entry has no catalog identity, so it is named
// by its own directory — enough to keep its declarations in one layer of
// their own, and unaddressable by a qualified reference, which is exactly
// what a plugin with no catalog identity is.
func (c *Config) pluginLayerOf(sourcePath string) string {
	for _, mounted := range c.Plugins {
		if under(sourcePath, mounted.Dir) {
			return mounted.ID
		}
	}
	for _, dir := range c.PluginDirs {
		if under(sourcePath, dir) {
			return dir
		}
	}
	return ""
}

func under(path, dir string) bool {
	return dir != "" && strings.HasPrefix(path, dir+string(filepath.Separator))
}

// pluginOwnership turns a plugin layer's identity into the reference
// ownership a declaration in that layer carries. A catalog id is
// `<alias>/<plugin path>`, and the reference form of that path is dotted.
func pluginOwnership(layer string) lang.Ownership {
	if layer == "" {
		return lang.Ownership{}
	}
	if filepath.IsAbs(layer) {
		// A directory-named layer: no alias exists to address it by, so the
		// path alone identifies it and no dotted reference can name it.
		return lang.Ownership{IsPlugin: true, Path: layer}
	}
	alias, path, found := strings.Cut(layer, "/")
	if !found {
		return lang.Ownership{IsPlugin: true, Alias: layer}
	}
	return lang.Ownership{IsPlugin: true, Alias: alias, Path: strings.ReplaceAll(path, "/", ".")}
}
