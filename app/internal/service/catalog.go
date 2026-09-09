package service

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/procexec"
)

// PluginPaths bundles the three on-disk locations the catalog and plugin
// subcommands read and write, so tests can point them at a scratch
// directory instead of the real ~/.config/plect and ~/.cache/plect.
type PluginPaths struct {
	CatalogsPath string
	LockfilePath string
	CacheRoot    string
}

// DefaultPluginPaths resolves the real, user-owned catalog/plugin paths.
func DefaultPluginPaths() (PluginPaths, error) {
	catalogsPath, err := plugins.DefaultCatalogsPath()
	if err != nil {
		return PluginPaths{}, err
	}
	lockPath, err := plugins.DefaultLockfilePath()
	if err != nil {
		return PluginPaths{}, err
	}
	cacheRoot, err := plugins.DefaultCacheRoot()
	if err != nil {
		return PluginPaths{}, err
	}
	return PluginPaths{CatalogsPath: catalogsPath, LockfilePath: lockPath, CacheRoot: cacheRoot}, nil
}

// CatalogAddParams are the inputs to `plect catalog add`.
type CatalogAddParams struct {
	Alias    string
	Source   string
	Subdir   string // catalog-relative subdirectory of the fetched source that becomes the catalog root; empty means the source root itself.
	Revision string // required for a git source; ignored otherwise.
}

// CatalogAddPreview is what fetching params.Source resolved to, before any
// persistence — shown to the user for confirmation.
type CatalogAddPreview struct {
	Alias            string
	Source           string
	ResolvedRevision string // empty for a path/path+editable source.
	Description      string
	Plugins          []string
}

// PreviewCatalogAdd fetches params.Source — network/disk I/O, no
// persistence — so the caller (the CLI) can show the user the exact
// source, resolved lock coordinate, manifest description, and published
// plugin paths before asking for confirmation. Returns the raw
// plugins.FetchedCatalog too: CommitCatalogAdd needs it and must not
// re-fetch (a second fetch could observe different content at a moving
// git ref).
func PreviewCatalogAdd(ctx context.Context, paths PluginPaths, params CatalogAddParams) (*CatalogAddPreview, *plugins.FetchedCatalog, error) {
	if strings.TrimSpace(params.Alias) == "" {
		return nil, nil, &Error{Code: ErrInvalidInput, Message: "alias is required"}
	}
	if strings.TrimSpace(params.Source) == "" {
		return nil, nil, &Error{Code: ErrInvalidInput, Message: "source is required"}
	}
	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		return nil, nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if _, exists := registrations.Find(params.Alias); exists {
		return nil, nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("catalog alias %q is already registered", params.Alias)}
	}

	fetched, err := plugins.FetchCatalog(ctx, procexec.Default, params.Source, params.Revision, params.Subdir, paths.CacheRoot)
	if err != nil {
		return nil, nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}

	return &CatalogAddPreview{
		Alias:            params.Alias,
		Source:           params.Source,
		ResolvedRevision: fetched.ResolvedRevision,
		Description:      fetched.Manifest.Description,
		Plugins:          fetched.Manifest.Plugins,
	}, &fetched, nil
}

// CatalogAddResult reports what CommitCatalogAdd persisted.
type CatalogAddResult struct {
	Alias            string
	ResolvedRevision string
}

// CommitCatalogAdd persists a fetch already resolved by PreviewCatalogAdd:
// it writes a new `[[catalogs]]` entry (alias, revision-free source, empty
// plugins list) to catalogs.toml, and a catalog lock record to plect.lock.
// The caller is responsible for having obtained interactive confirmation
// (or an explicit non-interactive override) before calling this — it does
// not ask.
func CommitCatalogAdd(paths PluginPaths, params CatalogAddParams, fetched *plugins.FetchedCatalog) (*CatalogAddResult, error) {
	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if _, exists := registrations.Find(params.Alias); exists {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("catalog alias %q is already registered", params.Alias)}
	}
	registrations.Catalogs = append(registrations.Catalogs, plugins.CatalogEntry{
		Alias:   params.Alias,
		Source:  params.Source,
		Subdir:  params.Subdir,
		Plugins: []string{},
	})

	lock, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	lock.PutCatalog(plugins.CatalogLockRecord{
		Alias:                   params.Alias,
		CatalogSource:           params.Source,
		Subdir:                  params.Subdir,
		CatalogResolvedRevision: fetched.ResolvedRevision,
	})

	if err := plugins.SaveCatalogRegistrations(paths.CatalogsPath, registrations); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if err := plugins.SaveLockfile(paths.LockfilePath, lock); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}

	return &CatalogAddResult{Alias: params.Alias, ResolvedRevision: fetched.ResolvedRevision}, nil
}

// CatalogUpdateParams are the inputs to `plect catalog update`.
type CatalogUpdateParams struct {
	Alias    string
	Revision string // required for a git-sourced catalog; must be empty for a path source.
}

// CatalogUpdateResult reports what CatalogUpdate persisted.
type CatalogUpdateResult struct {
	Alias            string
	ResolvedRevision string
	UpdatedPlugins   []string
}

// CatalogUpdate fetches an already-registered catalog at an explicit new
// revision (git) or re-resolves its current path content (locked path),
// then repoints every currently enabled plugin from that catalog to the new
// snapshot with fresh per-plugin lock entries (fresh content hash, rebuilt
// executables). catalogs.toml itself is untouched — the source stays
// revision-free.
func CatalogUpdate(ctx context.Context, paths PluginPaths, params CatalogUpdateParams) (*CatalogUpdateResult, error) {
	if strings.TrimSpace(params.Alias) == "" {
		return nil, &Error{Code: ErrInvalidInput, Message: "alias is required"}
	}
	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	entry, ok := registrations.Find(params.Alias)
	if !ok {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("catalog alias %q is not registered; run `plect catalog add` first", params.Alias)}
	}

	scheme, _, err := plugins.ParseSource(entry.Source)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	switch {
	case scheme.IsGit() && params.Revision == "":
		return nil, &Error{Code: ErrInvalidInput, Message: "`--revision` is required to update a git-sourced catalog"}
	case !scheme.IsGit() && params.Revision != "":
		return nil, &Error{Code: ErrInvalidInput, Message: "`--revision` does not apply to a path-sourced catalog"}
	}

	lock, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if err := plugins.CheckCatalogNotDrifted(entry, lock); err != nil {
		return nil, &Error{Code: ErrInvalidInput, Message: err.Error()}
	}

	fetched, err := plugins.FetchCatalog(ctx, procexec.Default, entry.Source, params.Revision, entry.Subdir, paths.CacheRoot)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	lock.PutCatalog(plugins.CatalogLockRecord{
		Alias:                   params.Alias,
		CatalogSource:           entry.Source,
		Subdir:                  entry.Subdir,
		CatalogResolvedRevision: fetched.ResolvedRevision,
	})

	updated := make([]string, 0, len(entry.Plugins))
	for _, path := range entry.Plugins {
		if !stringSliceContains(fetched.Manifest.Plugins, path) {
			return nil, &Error{Code: ErrExecutionFailed, Message: fmt.Sprintf("catalog %q: enabled plugin %q is no longer published by the new snapshot", params.Alias, path)}
		}
		locked, err := lockPluginAtPath(ctx, &fetched, entry, params.Alias, path)
		if err != nil {
			return nil, err
		}
		lock.PutPlugin(locked)
		updated = append(updated, path)
	}

	if err := plugins.SaveLockfile(paths.LockfilePath, lock); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	sort.Strings(updated)
	return &CatalogUpdateResult{Alias: params.Alias, ResolvedRevision: fetched.ResolvedRevision, UpdatedPlugins: updated}, nil
}

// lockPluginAtPath builds a fresh PluginLockEntry for the plugin at path
// inside an already-fetched catalog snapshot: it runs any declared build
// commands and computes the content hash used for later tamper detection.
func lockPluginAtPath(ctx context.Context, fetched *plugins.FetchedCatalog, entry plugins.CatalogEntry, alias, path string) (plugins.PluginLockEntry, error) {
	scheme, _, err := plugins.ParseSource(entry.Source)
	if err != nil {
		return plugins.PluginLockEntry{}, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	dir := filepath.Join(fetched.Root, path)
	m, err := plugins.LoadManifest(dir)
	if err != nil {
		return plugins.PluginLockEntry{}, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if err := plugins.RunBuilds(ctx, procexec.Default, dir, m); err != nil {
		return plugins.PluginLockEntry{}, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	// An editable catalog is never pinned or verified (see
	// plugins.VerifyAndMountPlugin): recording a content hash here would
	// falsely imply a reproducibility guarantee the design explicitly
	// disclaims for it.
	var hash string
	if !scheme.IsEditable() {
		hash, err = plugins.HashTreeExcluding(dir, plugins.BuildOutputPaths(m))
		if err != nil {
			return plugins.PluginLockEntry{}, &Error{Code: ErrExecutionFailed, Message: err.Error()}
		}
	}
	return plugins.PluginLockEntry{
		ID:                      alias + "/" + path,
		CatalogAlias:            alias,
		CatalogSource:           entry.Source,
		CatalogResolvedRevision: fetched.ResolvedRevision,
		Path:                    path,
		ContentHash:             hash,
		Editable:                scheme.IsEditable(),
		Version:                 m.Version,
		PlectMinVersion:         m.PlectMinVersion,
	}, nil
}

// CatalogRemoveResult reports what CatalogRemove disabled.
type CatalogRemoveResult struct {
	Alias           string
	DisabledPlugins []string
}

// PreviewCatalogRemove reports which plugins would be disabled by removing
// alias, without changing anything — the CLI shows this before confirming.
func PreviewCatalogRemove(paths PluginPaths, alias string) (*CatalogRemoveResult, error) {
	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	entry, ok := registrations.Find(alias)
	if !ok {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("catalog alias %q is not registered", alias)}
	}
	return &CatalogRemoveResult{Alias: alias, DisabledPlugins: entry.Plugins}, nil
}

// CommitCatalogRemove removes alias's registration, catalog lock record,
// and every plugin lock entry it owns. The caller is responsible for
// having obtained confirmation first.
func CommitCatalogRemove(paths PluginPaths, alias string) (*CatalogRemoveResult, error) {
	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	var kept []plugins.CatalogEntry
	var removed *plugins.CatalogEntry
	for _, c := range registrations.Catalogs {
		if c.Alias == alias {
			entry := c
			removed = &entry
			continue
		}
		kept = append(kept, c)
	}
	if removed == nil {
		return nil, &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("catalog alias %q is not registered", alias)}
	}
	registrations.Catalogs = kept

	lock, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	var keptCatalogs []plugins.CatalogLockRecord
	for _, c := range lock.Catalogs {
		if c.Alias != alias {
			keptCatalogs = append(keptCatalogs, c)
		}
	}
	lock.Catalogs = keptCatalogs
	var keptPlugins []plugins.PluginLockEntry
	for _, p := range lock.Plugins {
		if p.CatalogAlias != alias {
			keptPlugins = append(keptPlugins, p)
		}
	}
	lock.Plugins = keptPlugins

	if err := plugins.SaveCatalogRegistrations(paths.CatalogsPath, registrations); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if err := plugins.SaveLockfile(paths.LockfilePath, lock); err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	return &CatalogRemoveResult{Alias: alias, DisabledPlugins: removed.Plugins}, nil
}

// CatalogListEntry is one registered catalog's validation and enabled-plugin
// state, for `plect catalog list`.
type CatalogListEntry struct {
	Alias            string
	Source           string
	Subdir           string // catalog-relative subdirectory that is the catalog root; empty means the source root itself.
	ResolvedRevision string
	Status           string // "ok" or the resolution error's message.
	EnabledPlugins   []string
}

// CatalogList reports every registered catalog's state without mounting
// anything into a live config.
func CatalogList(paths PluginPaths) ([]CatalogListEntry, error) {
	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	lock, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}

	entries := make([]CatalogListEntry, 0, len(registrations.Catalogs))
	for _, c := range registrations.Catalogs {
		record, _ := lock.FindCatalog(c.Alias)
		e := CatalogListEntry{
			Alias:            c.Alias,
			Source:           c.Source,
			Subdir:           c.Subdir,
			ResolvedRevision: record.CatalogResolvedRevision,
			Status:           "ok",
			EnabledPlugins:   c.Plugins,
		}
		if _, err := plugins.VerifyAndMountCatalog(c, lock, paths.CacheRoot); err != nil {
			e.Status = err.Error()
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Alias < entries[j].Alias })
	return entries, nil
}

func stringSliceContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
