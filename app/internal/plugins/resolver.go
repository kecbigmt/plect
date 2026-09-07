package plugins

import (
	"fmt"
	"path/filepath"
)

// Mounted is one catalog-qualified plugin resolved and verified, ready to
// be mounted as a read-only config base layer.
type Mounted struct {
	ID              string
	Dir             string
	Manifest        Manifest
	NonReproducible bool
}

// ErrMissingCatalogLock is returned when a non-editable catalog has no
// corresponding plect.lock record.
type ErrMissingCatalogLock struct{ Alias string }

func (e *ErrMissingCatalogLock) Error() string {
	return fmt.Sprintf("catalog %q: no plect.lock entry; run `plect catalog add` or `plect catalog update`", e.Alias)
}

// ErrCatalogSourceDrift is returned when catalogs.toml's declared source or
// subdir no longer matches what plect.lock recorded — a human hand-edited
// the registration without re-running catalog add/update. subdir is
// included in this check because it changes what subtree is trusted exactly
// the way source does: an unnoticed subdir edit would resolve a plugin's
// lock entry against a catalog root nobody re-confirmed. `plect catalog
// update` is deliberately not the suggested fix: it re-reads the same
// drifted catalogs.toml values (CheckCatalogNotDrifted blocks it too), so
// the only way to actually accept a changed source/subdir is to remove and
// re-add the catalog — a fresh, explicit `plect catalog add` trust
// confirmation.
type ErrCatalogSourceDrift struct{ Alias string }

func (e *ErrCatalogSourceDrift) Error() string {
	return fmt.Sprintf("catalog %q: declared source/subdir does not match plect.lock; run `plect catalog remove %s` then `plect catalog add` to accept the change", e.Alias, e.Alias)
}

// CheckCatalogNotDrifted reports ErrCatalogSourceDrift if entry's source or
// subdir disagrees with an already-existing catalog lock record for its
// alias. Every command that trusts catalogs.toml's current source/subdir to
// fetch and (re)write plect.lock — catalog update, plugin add, plugin
// update — must call this before doing either: without it, a hand-edited
// catalogs.toml would silently launder its new source/subdir into a trusted
// lock entry the next time any plugin from that catalog is added or
// updated, bypassing the explicit re-confirmation `plect catalog add`
// exists to require. A missing lock record is not drift — nothing has been
// trusted for this alias yet, so the caller's own fetch is the first trust
// act.
func CheckCatalogNotDrifted(entry CatalogEntry, lock *Lockfile) error {
	record, ok := lock.FindCatalog(entry.Alias)
	if !ok {
		return nil
	}
	if record.CatalogSource != entry.Source || record.Subdir != entry.Subdir {
		return &ErrCatalogSourceDrift{Alias: entry.Alias}
	}
	return nil
}

// ErrMissingPluginLock is returned when a non-editable plugin has no
// corresponding plect.lock entry.
type ErrMissingPluginLock struct{ ID string }

func (e *ErrMissingPluginLock) Error() string {
	return fmt.Sprintf("plugin %q: no plect.lock entry; run `plect plugin add` or `plect plugin update`", e.ID)
}

// ErrIncompatible is returned when the running plect version does not
// satisfy a plugin's plect_min_version.
type ErrIncompatible struct{ ID, Required, Running string }

func (e *ErrIncompatible) Error() string {
	return fmt.Sprintf("plugin %q requires plect >= %s, running %s", e.ID, e.Required, e.Running)
}

// ResolvedCatalog is a catalog root resolved from local state and validated
// against its own catalog.toml. For a git catalog, Root/Manifest are only
// the snapshot the shared catalog lock record currently points at — proof
// the registration itself is intact — never a plugin's mount point or the
// source of truth for whether a given plugin is still published:
// VerifyAndMountPlugin re-resolves both per plugin for a git source, since
// `plugin update` repoints one plugin's own lock entry without touching its
// siblings, so two plugins from the same catalog can be legitimately pinned
// to different commits (each governed by that commit's own catalog.toml)
// at once.
type ResolvedCatalog struct {
	Alias           string
	Source          string
	Subdir          string
	Root            string
	Manifest        CatalogManifest
	NonReproducible bool
}

// VerifyAndMountCatalog resolves entry to a validated catalog root, entirely
// from local state: it never fetches over the network, so it is safe and
// fast to run on every config load. A registration itself is the trust act
// (catalogs.toml only ever contains what a human confirmed through `plect
// catalog add`), so this performs no separate trust check — only content
// verification against plect.lock, except for an editable path catalog,
// which is explicitly non-reproducible.
func VerifyAndMountCatalog(entry CatalogEntry, lock *Lockfile, cacheRoot string) (ResolvedCatalog, error) {
	scheme, rest, err := ParseSource(entry.Source)
	if err != nil {
		return ResolvedCatalog{}, err
	}

	if scheme.IsEditable() {
		root, _, err := resolvePathTree(rest)
		if err != nil {
			return ResolvedCatalog{}, err
		}
		root, err = ResolveCatalogSubdir(root, entry.Subdir)
		if err != nil {
			return ResolvedCatalog{}, err
		}
		manifest, err := LoadCatalogManifest(root)
		if err != nil {
			return ResolvedCatalog{}, err
		}
		return ResolvedCatalog{Alias: entry.Alias, Source: entry.Source, Subdir: entry.Subdir, Root: root, Manifest: manifest, NonReproducible: true}, nil
	}

	record, ok := lock.FindCatalog(entry.Alias)
	if !ok {
		return ResolvedCatalog{}, &ErrMissingCatalogLock{Alias: entry.Alias}
	}
	if record.CatalogSource != entry.Source || record.Subdir != entry.Subdir {
		return ResolvedCatalog{}, &ErrCatalogSourceDrift{Alias: entry.Alias}
	}

	var root string
	switch {
	case scheme.IsGit():
		if record.CatalogResolvedRevision == "" {
			return ResolvedCatalog{}, &ErrMissingCatalogLock{Alias: entry.Alias}
		}
		root = CacheDir(cacheRoot, entry.Source, record.CatalogResolvedRevision)
	case scheme == SchemePath:
		root, _, err = resolvePathTree(rest)
		if err != nil {
			return ResolvedCatalog{}, err
		}
	default:
		return ResolvedCatalog{}, fmt.Errorf("catalog %q: source %q: unsupported scheme", entry.Alias, entry.Source)
	}
	root, err = ResolveCatalogSubdir(root, entry.Subdir)
	if err != nil {
		return ResolvedCatalog{}, err
	}

	manifest, err := LoadCatalogManifest(root)
	if err != nil {
		return ResolvedCatalog{}, err
	}
	return ResolvedCatalog{Alias: entry.Alias, Source: entry.Source, Subdir: entry.Subdir, Root: root, Manifest: manifest}, nil
}

// ErrPluginNotListed is returned when pluginPath is enabled in
// catalogs.toml but is not listed by `plugins` in the catalog.toml that
// governs the commit it is actually pinned to.
type ErrPluginNotListed struct {
	Alias string
	Path  string
}

func (e *ErrPluginNotListed) Error() string {
	return fmt.Sprintf("catalog %q: enabled plugin path %q is not listed in catalog.toml at its locked commit", e.Alias, e.Path)
}

// VerifyAndMountPlugin resolves one plugin at pluginPath belonging to an
// already-resolved catalog, verifying its own lock entry's content hash
// against the mounted directory — local-only, Load()-time safe.
//
// For a non-editable git catalog, both the mount directory and the
// catalog.toml membership check are resolved from the plugin's OWN lock
// entry's CatalogResolvedRevision, via cacheRoot — deliberately never from
// catalog.Root/catalog.Manifest. `plugin update` repoints only the target
// plugin's lock entry, so a sibling plugin from the same catalog can still
// be pinned to an older commit whose catalog.toml differs from the
// catalog's current shared snapshot (a newer commit may have dropped that
// sibling from `plugins` entirely). Checking membership against
// catalog.Manifest here would reject a sibling that is still validly
// published at its own locked commit, and reusing catalog.Root as the
// mount point would silently drift it onto a commit it was never verified
// against.
func VerifyAndMountPlugin(catalog ResolvedCatalog, pluginPath string, cacheRoot string, lock *Lockfile, currentPlectVersion string) (Mounted, error) {
	id := catalog.Alias + "/" + pluginPath

	if catalog.NonReproducible {
		if !containsString(catalog.Manifest.Plugins, pluginPath) {
			return Mounted{}, &ErrPluginNotListed{Alias: catalog.Alias, Path: pluginPath}
		}
		dir := filepath.Join(catalog.Root, pluginPath)
		m, err := LoadManifest(dir)
		if err != nil {
			return Mounted{}, err
		}
		return finishPluginMount(id, dir, m, true, currentPlectVersion)
	}

	entry, ok := lock.FindPlugin(id)
	if !ok {
		return Mounted{}, &ErrMissingPluginLock{ID: id}
	}

	scheme, _, err := ParseSource(catalog.Source)
	if err != nil {
		return Mounted{}, err
	}
	pluginCatalogRoot := catalog.Root
	pluginCatalogManifest := catalog.Manifest
	if scheme.IsGit() {
		if entry.CatalogResolvedRevision == "" {
			return Mounted{}, &ErrMissingPluginLock{ID: id}
		}
		pluginCatalogRoot, err = ResolveCatalogSubdir(CacheDir(cacheRoot, catalog.Source, entry.CatalogResolvedRevision), catalog.Subdir)
		if err != nil {
			return Mounted{}, err
		}
		pluginCatalogManifest, err = LoadCatalogManifest(pluginCatalogRoot)
		if err != nil {
			return Mounted{}, err
		}
	}
	if !containsString(pluginCatalogManifest.Plugins, pluginPath) {
		return Mounted{}, &ErrPluginNotListed{Alias: catalog.Alias, Path: pluginPath}
	}
	dir := filepath.Join(pluginCatalogRoot, pluginPath)

	m, err := LoadManifest(dir)
	if err != nil {
		return Mounted{}, err
	}
	hash, err := HashTreeExcluding(dir, BuildOutputPaths(m))
	if err != nil {
		return Mounted{}, fmt.Errorf("plugin %q: %w", id, err)
	}
	if hash != entry.ContentHash {
		return Mounted{}, &ErrHashMismatch{Path: dir, Want: entry.ContentHash, Got: hash}
	}
	return finishPluginMount(id, dir, m, false, currentPlectVersion)
}

func finishPluginMount(id, dir string, m Manifest, nonReproducible bool, currentPlectVersion string) (Mounted, error) {
	satisfied, err := AtLeast(currentPlectVersion, m.PlectMinVersion)
	if err != nil {
		return Mounted{}, fmt.Errorf("plugin %q: %w", id, err)
	}
	if !satisfied {
		return Mounted{}, &ErrIncompatible{ID: id, Required: m.PlectMinVersion, Running: currentPlectVersion}
	}
	return Mounted{ID: id, Dir: dir, Manifest: m, NonReproducible: nonReproducible}, nil
}

// VerifyAndMountAll resolves every registered catalog and its enabled
// plugins, entirely from local state, in declaration order. Catalog-level
// resolution (VerifyAndMountCatalog) only needs to succeed enough to prove
// the registration itself is intact; per-plugin `plugins` membership is
// checked inside VerifyAndMountPlugin against each plugin's own locked
// commit, not repeated here against the catalog's shared snapshot.
func VerifyAndMountAll(registrations *CatalogRegistrations, lock *Lockfile, cacheRoot, currentPlectVersion string) ([]Mounted, error) {
	var out []Mounted
	for _, entry := range registrations.Catalogs {
		rc, err := VerifyAndMountCatalog(entry, lock, cacheRoot)
		if err != nil {
			return nil, fmt.Errorf("catalog %q: %w", entry.Alias, err)
		}
		for _, path := range entry.Plugins {
			mounted, err := VerifyAndMountPlugin(rc, path, cacheRoot, lock, currentPlectVersion)
			if err != nil {
				return nil, err
			}
			out = append(out, mounted)
		}
	}
	return out, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
