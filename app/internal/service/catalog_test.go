package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/plugins"
	"github.com/kecbigmt/plecture/app/internal/testpath"
)

func catalogTestPaths(t *testing.T) PluginPaths {
	t.Helper()
	dir := t.TempDir()
	return PluginPaths{
		CatalogsPath: filepath.Join(dir, "catalogs.toml"),
		LockfilePath: filepath.Join(dir, "plect.lock"),
		CacheRoot:    filepath.Join(dir, "cache"),
	}
}

func writeCatalogSource(t *testing.T, plugins map[string]string) string {
	t.Helper()
	dir := testpath.Real(t, t.TempDir())
	var names []string
	for name := range plugins {
		names = append(names, name)
	}
	manifest := "schema_version = 2\nplugins = ["
	for i, name := range names {
		if i > 0 {
			manifest += ", "
		}
		manifest += "\"" + name + "\""
	}
	manifest += "]\n"
	if err := os.WriteFile(filepath.Join(dir, "catalog.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, minVersion := range plugins {
		pluginDir := filepath.Join(dir, name)
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "schema_version = 2\nplect_min_version = \"" + minVersion + "\"\n"
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPreviewCatalogAdd_RequiresAliasAndSource(t *testing.T) {
	paths := catalogTestPaths(t)
	if _, _, err := PreviewCatalogAdd(context.Background(), paths, CatalogAddParams{}); err == nil {
		t.Fatal("want error for empty alias/source, got nil")
	}
}

func TestPreviewCatalogAdd_ResolvesCatalog(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})

	preview, fetched, err := PreviewCatalogAdd(context.Background(), paths, CatalogAddParams{Alias: "local", Source: "path+editable://" + src})
	if err != nil {
		t.Fatalf("PreviewCatalogAdd: unexpected error: %v", err)
	}
	if preview.Alias != "local" || len(preview.Plugins) != 1 || preview.Plugins[0] != "okf" {
		t.Fatalf("preview = %+v", preview)
	}
	if fetched.Manifest.Plugins[0] != "okf" {
		t.Fatalf("fetched = %+v", fetched)
	}
}

// writeCatalogSourceWithSubdir is writeCatalogSource with catalog.toml and
// its plugin directories nested one level under dirName instead of at the
// source root, so tests can register with --subdir.
func writeCatalogSourceWithSubdir(t *testing.T, dirName string, plugins map[string]string) string {
	t.Helper()
	root := writeCatalogSource(t, nil)
	catalogRoot := filepath.Join(root, dirName)
	var names []string
	for name := range plugins {
		names = append(names, name)
	}
	manifest := "schema_version = 2\nplugins = ["
	for i, name := range names {
		if i > 0 {
			manifest += ", "
		}
		manifest += "\"" + name + "\""
	}
	manifest += "]\n"
	if err := os.MkdirAll(catalogRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogRoot, "catalog.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, minVersion := range plugins {
		pluginDir := filepath.Join(catalogRoot, name)
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		content := "schema_version = 2\nplect_min_version = \"" + minVersion + "\"\n"
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPreviewCatalogAdd_SubdirScopesTrustSpace(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSourceWithSubdir(t, "plugins", map[string]string{"okf": "0.0.0"})

	preview, fetched, err := PreviewCatalogAdd(context.Background(), paths, CatalogAddParams{
		Alias: "local", Source: "path+editable://" + src, Subdir: "plugins",
	})
	if err != nil {
		t.Fatalf("PreviewCatalogAdd: unexpected error: %v", err)
	}
	if len(preview.Plugins) != 1 || preview.Plugins[0] != "okf" {
		t.Fatalf("preview.Plugins = %v", preview.Plugins)
	}
	wantRoot := filepath.Join(src, "plugins")
	if fetched.Root != wantRoot {
		t.Errorf("fetched.Root = %q, want %q", fetched.Root, wantRoot)
	}
}

func TestCommitCatalogAdd_PersistsSubdir(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSourceWithSubdir(t, "plugins", map[string]string{"okf": "0.0.0"})
	params := CatalogAddParams{Alias: "local", Source: "path+editable://" + src, Subdir: "plugins"}

	_, fetched, err := PreviewCatalogAdd(context.Background(), paths, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CommitCatalogAdd(paths, params, fetched); err != nil {
		t.Fatalf("CommitCatalogAdd: unexpected error: %v", err)
	}

	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations.Catalogs) != 1 || registrations.Catalogs[0].Subdir != "plugins" {
		t.Fatalf("Catalogs = %+v, want one entry with subdir=\"plugins\"", registrations.Catalogs)
	}
}

func TestPluginAdd_SubdirScopedCatalog(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSourceWithSubdir(t, "plugins", map[string]string{"okf": "0.0.0"})
	params := CatalogAddParams{Alias: "local", Source: "path://" + src, Subdir: "plugins"}
	_, fetched, err := PreviewCatalogAdd(context.Background(), paths, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CommitCatalogAdd(paths, params, fetched); err != nil {
		t.Fatal(err)
	}

	result, err := PluginAdd(context.Background(), paths, "local/okf")
	if err != nil {
		t.Fatalf("PluginAdd: unexpected error: %v", err)
	}
	if result.ID != "local/okf" {
		t.Errorf("ID = %q", result.ID)
	}
}

func TestCommitCatalogAdd_PersistsRegistrationAndLock(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	params := CatalogAddParams{Alias: "local", Source: "path+editable://" + src}

	_, fetched, err := PreviewCatalogAdd(context.Background(), paths, params)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CommitCatalogAdd(paths, params, fetched)
	if err != nil {
		t.Fatalf("CommitCatalogAdd: unexpected error: %v", err)
	}
	if got.Alias != "local" {
		t.Errorf("Alias = %q", got.Alias)
	}

	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations.Catalogs) != 1 || len(registrations.Catalogs[0].Plugins) != 0 {
		t.Fatalf("Catalogs = %+v, want one entry with an empty plugins list", registrations.Catalogs)
	}

	lock, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lock.FindCatalog("local"); !ok {
		t.Fatal("catalog lock record not written")
	}
}

func TestCommitCatalogAdd_AlreadyRegisteredFailsLoud(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	params := CatalogAddParams{Alias: "local", Source: "path+editable://" + src}

	_, fetched, err := PreviewCatalogAdd(context.Background(), paths, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CommitCatalogAdd(paths, params, fetched); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitCatalogAdd(paths, params, fetched); err == nil {
		t.Fatal("want error for an already-registered alias, got nil")
	}
}

func addTestCatalog(t *testing.T, paths PluginPaths, alias, source string) {
	t.Helper()
	params := CatalogAddParams{Alias: alias, Source: source}
	_, fetched, err := PreviewCatalogAdd(context.Background(), paths, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CommitCatalogAdd(paths, params, fetched); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogUpdate_UnregisteredAliasFailsLoud(t *testing.T) {
	paths := catalogTestPaths(t)
	if _, err := CatalogUpdate(context.Background(), paths, CatalogUpdateParams{Alias: "missing"}); err == nil {
		t.Fatal("want error for an unregistered alias, got nil")
	}
}

func TestCatalogUpdate_PathSourceRejectsRevisionFlag(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	addTestCatalog(t, paths, "local", "path://"+src)

	if _, err := CatalogUpdate(context.Background(), paths, CatalogUpdateParams{Alias: "local", Revision: "v1"}); err == nil {
		t.Fatal("want error when --revision is passed for a path-sourced catalog, got nil")
	}
}

func TestCatalogUpdate_RepointsEnabledPlugins(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	addTestCatalog(t, paths, "local", "path://"+src)
	if _, err := PluginAdd(context.Background(), paths, "local/okf"); err != nil {
		t.Fatal(err)
	}
	before, ok := (func() (plugins.PluginLockEntry, bool) {
		lf, err := plugins.LoadLockfile(paths.LockfilePath)
		if err != nil {
			t.Fatal(err)
		}
		return lf.FindPlugin("local/okf")
	})()
	if !ok {
		t.Fatal("expected local/okf to be locked after PluginAdd")
	}

	// Change source content, then update the catalog.
	if err := os.WriteFile(filepath.Join(src, "okf", "plugin.toml"), []byte("schema_version = 2\nplect_min_version = \"0.0.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := CatalogUpdate(context.Background(), paths, CatalogUpdateParams{Alias: "local"})
	if err != nil {
		t.Fatalf("CatalogUpdate: unexpected error: %v", err)
	}
	if len(result.UpdatedPlugins) != 1 || result.UpdatedPlugins[0] != "okf" {
		t.Fatalf("UpdatedPlugins = %v", result.UpdatedPlugins)
	}

	lf, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := lf.FindPlugin("local/okf")
	if !ok {
		t.Fatal("local/okf lock entry disappeared after update")
	}
	if after.ContentHash == before.ContentHash {
		t.Fatal("CatalogUpdate did not re-hash the changed plugin content")
	}
}

// TestCatalogUpdate_RejectsHandEditedSubdirDrift is a regression test:
// CatalogUpdate used to read catalogs.toml's current `subdir` straight into
// a fresh fetch and lock write with no comparison against what was already
// locked, so a hand-edited subdir would get silently trusted on the next
// update — never routing through the interactive `plect catalog add`
// confirmation that changing a trusted subtree requires.
func TestCatalogUpdate_RejectsHandEditedSubdirDrift(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	addTestCatalog(t, paths, "local", "path://"+src)

	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range registrations.Catalogs {
		if registrations.Catalogs[i].Alias == "local" {
			registrations.Catalogs[i].Subdir = "sub"
		}
	}
	if err := plugins.SaveCatalogRegistrations(paths.CatalogsPath, registrations); err != nil {
		t.Fatal(err)
	}

	_, err = CatalogUpdate(context.Background(), paths, CatalogUpdateParams{Alias: "local"})
	if err == nil || !strings.Contains(err.Error(), "does not match plect.lock") {
		t.Fatalf("CatalogUpdate after a hand-edited subdir: err = %v, want a source/subdir drift error", err)
	}
}

func TestPreviewCatalogRemove_ReportsDisabledPlugins(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	addTestCatalog(t, paths, "local", "path://"+src)
	if _, err := PluginAdd(context.Background(), paths, "local/okf"); err != nil {
		t.Fatal(err)
	}

	preview, err := PreviewCatalogRemove(paths, "local")
	if err != nil {
		t.Fatalf("PreviewCatalogRemove: unexpected error: %v", err)
	}
	if len(preview.DisabledPlugins) != 1 || preview.DisabledPlugins[0] != "okf" {
		t.Fatalf("DisabledPlugins = %v", preview.DisabledPlugins)
	}
}

func TestCommitCatalogRemove_RemovesRegistrationAndLocks(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	addTestCatalog(t, paths, "local", "path://"+src)
	if _, err := PluginAdd(context.Background(), paths, "local/okf"); err != nil {
		t.Fatal(err)
	}

	if _, err := CommitCatalogRemove(paths, "local"); err != nil {
		t.Fatalf("CommitCatalogRemove: unexpected error: %v", err)
	}

	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations.Catalogs) != 0 {
		t.Fatalf("Catalogs = %+v, want empty", registrations.Catalogs)
	}
	lock, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Catalogs) != 0 || len(lock.Plugins) != 0 {
		t.Fatalf("Lockfile = %+v, want empty", lock)
	}
}

func TestCommitCatalogRemove_UnregisteredAliasFailsLoud(t *testing.T) {
	paths := catalogTestPaths(t)
	if _, err := CommitCatalogRemove(paths, "missing"); err == nil {
		t.Fatal("want error for an unregistered alias, got nil")
	}
}

func TestCatalogList_ReportsRegisteredCatalogs(t *testing.T) {
	paths := catalogTestPaths(t)
	src := writeCatalogSource(t, map[string]string{"okf": "0.0.0"})
	addTestCatalog(t, paths, "local", "path+editable://"+src)
	if _, err := PluginAdd(context.Background(), paths, "local/okf"); err != nil {
		t.Fatal(err)
	}

	got, err := CatalogList(paths)
	if err != nil {
		t.Fatalf("CatalogList: unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Alias != "local" || got[0].Status != "ok" || len(got[0].EnabledPlugins) != 1 {
		t.Fatalf("CatalogList = %+v", got)
	}
}

// TestCatalogAdd_ThisRepositoryWithSubdir is the GWT this repository's own
// issue tracker asked for directly: register this repository as a
// path-sourced catalog scoped to its plugins/ subtree via --subdir, enable
// github, and confirm both the identity has no plugins/ prefix and the
// mounted plugin directory never resolves outside plugins/.
func TestCatalogAdd_ThisRepositoryWithSubdir(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	paths := catalogTestPaths(t)
	params := CatalogAddParams{Alias: "official", Source: "path+editable://" + repoRoot, Subdir: "plugins"}

	preview, fetched, err := PreviewCatalogAdd(context.Background(), paths, params)
	if err != nil {
		t.Fatalf("PreviewCatalogAdd: unexpected error: %v", err)
	}
	if !stringSliceContains(preview.Plugins, "github") {
		t.Fatalf("preview.Plugins = %v, want it to contain %q", preview.Plugins, "github")
	}
	wantRoot := filepath.Join(repoRoot, "plugins")
	if fetched.Root != wantRoot {
		t.Errorf("fetched.Root = %q, want %q", fetched.Root, wantRoot)
	}

	if _, err := CommitCatalogAdd(paths, params, fetched); err != nil {
		t.Fatalf("CommitCatalogAdd: unexpected error: %v", err)
	}
	result, err := PluginAdd(context.Background(), paths, "official/github")
	if err != nil {
		t.Fatalf("PluginAdd: unexpected error: %v", err)
	}
	if result.ID != "official/github" {
		t.Errorf("ID = %q, want %q (no plugins/ prefix)", result.ID, "official/github")
	}
}

// TestCatalogFlow_MultiSegmentSubdir is the GWT the issue asked for
// directly: subdir = "path/to/plugins" (multi-segment) must work
// identically to a single-segment subdir across every flow that reads or
// writes it — add, lock, list, plugin add, update, and drift-check — with
// the containment validation behavior unchanged.
func TestCatalogFlow_MultiSegmentSubdir(t *testing.T) {
	paths := catalogTestPaths(t)
	multiSegment := filepath.Join("path", "to", "plugins")
	src := writeCatalogSourceWithSubdir(t, multiSegment, map[string]string{"okf": "0.0.0"})
	params := CatalogAddParams{Alias: "local", Source: "path://" + src, Subdir: multiSegment}

	// When: the catalog is added.
	preview, fetched, err := PreviewCatalogAdd(context.Background(), paths, params)
	if err != nil {
		t.Fatalf("PreviewCatalogAdd: unexpected error: %v", err)
	}
	wantRoot := filepath.Join(src, multiSegment)
	if fetched.Root != wantRoot {
		t.Fatalf("fetched.Root = %q, want %q (containment must resolve the full multi-segment path)", fetched.Root, wantRoot)
	}
	if !stringSliceContains(preview.Plugins, "okf") {
		t.Fatalf("preview.Plugins = %v, want it to contain %q", preview.Plugins, "okf")
	}
	if _, err := CommitCatalogAdd(paths, params, fetched); err != nil {
		t.Fatalf("CommitCatalogAdd: unexpected error: %v", err)
	}

	// Then: catalogs.toml and plect.lock both record the multi-segment
	// subdir verbatim (locked).
	registrations, err := plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations.Catalogs) != 1 || registrations.Catalogs[0].Subdir != multiSegment {
		t.Fatalf("Catalogs = %+v, want one entry with subdir=%q", registrations.Catalogs, multiSegment)
	}
	lock, err := plugins.LoadLockfile(paths.LockfilePath)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := lock.FindCatalog("local")
	if !ok || record.Subdir != multiSegment {
		t.Fatalf("catalog lock record = %+v, ok=%v, want subdir=%q", record, ok, multiSegment)
	}

	// When: the catalog is listed.
	entries, err := CatalogList(paths)
	if err != nil {
		t.Fatalf("CatalogList: unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Subdir != multiSegment || entries[0].Status != "ok" {
		t.Fatalf("CatalogList = %+v, want one ok entry with subdir=%q", entries, multiSegment)
	}

	// When: a plugin from it is enabled — containment must resolve the
	// mount point through the full multi-segment path, not just its last
	// component.
	added, err := PluginAdd(context.Background(), paths, "local/okf")
	if err != nil {
		t.Fatalf("PluginAdd: unexpected error: %v", err)
	}
	if added.ID != "local/okf" {
		t.Errorf("ID = %q, want %q", added.ID, "local/okf")
	}

	// When: the source content changes and the catalog is updated.
	pluginTomlPath := filepath.Join(src, multiSegment, "okf", "plugin.toml")
	if err := os.WriteFile(pluginTomlPath, []byte("schema_version = 2\nplect_min_version = \"0.0.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updateResult, err := CatalogUpdate(context.Background(), paths, CatalogUpdateParams{Alias: "local"})
	if err != nil {
		t.Fatalf("CatalogUpdate: unexpected error: %v", err)
	}
	if len(updateResult.UpdatedPlugins) != 1 || updateResult.UpdatedPlugins[0] != "okf" {
		t.Fatalf("UpdatedPlugins = %v", updateResult.UpdatedPlugins)
	}

	// Then: a hand-edited multi-segment subdir is rejected as drift, not
	// silently trusted by the next update.
	registrations, err = plugins.LoadCatalogRegistrations(paths.CatalogsPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range registrations.Catalogs {
		if registrations.Catalogs[i].Alias == "local" {
			registrations.Catalogs[i].Subdir = filepath.Join("other", "path")
		}
	}
	if err := plugins.SaveCatalogRegistrations(paths.CatalogsPath, registrations); err != nil {
		t.Fatal(err)
	}
	if _, err := CatalogUpdate(context.Background(), paths, CatalogUpdateParams{Alias: "local"}); err == nil || !strings.Contains(err.Error(), "does not match plect.lock") {
		t.Fatalf("CatalogUpdate after a hand-edited multi-segment subdir: err = %v, want a source/subdir drift error", err)
	}
}
