package plugins

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/procexec"
)

const testPlectVersion = "0.8.0"

func TestVerifyAndMountCatalog_GitSource_MissingLockFailsLoud(t *testing.T) {
	repo, _, _ := newLocalCatalogGitRepo(t)
	entry := CatalogEntry{Alias: "official", Source: "git+https://" + repo, Plugins: []string{"okf"}}

	_, err := VerifyAndMountCatalog(entry, &Lockfile{}, t.TempDir())
	var want *ErrMissingCatalogLock
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrMissingCatalogLock", err)
	}
}

func TestVerifyAndMountCatalog_GitSource_SourceDriftFailsLoud(t *testing.T) {
	repo, firstCommit, _ := newLocalCatalogGitRepo(t)
	entry := CatalogEntry{Alias: "official", Source: "git+https://" + repo, Plugins: []string{"okf"}}
	lock := &Lockfile{Catalogs: []CatalogLockRecord{{
		Alias: "official", CatalogSource: "git+https://different-source", CatalogResolvedRevision: firstCommit,
	}}}

	_, err := VerifyAndMountCatalog(entry, lock, t.TempDir())
	var want *ErrCatalogSourceDrift
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrCatalogSourceDrift", err)
	}
}

func TestVerifyAndMountCatalog_GitSource_SubdirDriftFailsLoud(t *testing.T) {
	repo, firstCommit, _ := newLocalCatalogGitRepo(t)
	entry := CatalogEntry{Alias: "official", Source: "git+https://" + repo, Subdir: "plugins", Plugins: []string{"okf"}}
	lock := &Lockfile{Catalogs: []CatalogLockRecord{{
		Alias: "official", CatalogSource: "git+https://" + repo, Subdir: "", CatalogResolvedRevision: firstCommit,
	}}}

	_, err := VerifyAndMountCatalog(entry, lock, t.TempDir())
	var want *ErrCatalogSourceDrift
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrCatalogSourceDrift", err)
	}
}

// newLocalCatalogGitRepoWithSubdir is newLocalCatalogGitRepo's catalog.toml
// nested one level under dirName instead of at the repo root, so tests can
// exercise a git-sourced catalog registered with --subdir.
func newLocalCatalogGitRepoWithSubdir(t *testing.T, dirName string) (repoDir, firstCommit string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
	dir := t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=plect-test", "GIT_AUTHOR_EMAIL=plect-test@example.test",
		"GIT_COMMITTER_NAME=plect-test", "GIT_COMMITTER_EMAIL=plect-test@example.test",
	)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	catalogRoot := filepath.Join(dir, dirName)
	writeCatalogManifest(t, catalogRoot, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(catalogRoot, "okf"))
	// A sibling directory outside dirName, unrelated to the catalog, so a
	// test can assert the resolved catalog root does not reach it.
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "notes.md"), []byte("outside the catalog trust space"), 0o644); err != nil {
		t.Fatal(err)
	}

	run("init", "--quiet", "--initial-branch=main")
	run("add", ".")
	run("commit", "--quiet", "-m", "v1")
	first := run("rev-parse", "HEAD")

	return dir, first
}

func TestVerifyAndMountCatalog_GitSource_SubdirScopesRoot(t *testing.T) {
	repo, firstCommit := newLocalCatalogGitRepoWithSubdir(t, "plugins")
	cacheRoot := t.TempDir()
	source := "git+https://" + repo
	if _, _, err := fetchGitCatalog(context.Background(), procexec.Default, source, repo, firstCommit, cacheRoot); err != nil {
		t.Fatal(err)
	}

	entry := CatalogEntry{Alias: "official", Source: source, Subdir: "plugins", Plugins: []string{"okf"}}
	lock := &Lockfile{Catalogs: []CatalogLockRecord{{Alias: "official", CatalogSource: source, Subdir: "plugins", CatalogResolvedRevision: firstCommit}}}

	rc, err := VerifyAndMountCatalog(entry, lock, cacheRoot)
	if err != nil {
		t.Fatalf("VerifyAndMountCatalog: unexpected error: %v", err)
	}
	wantRoot := filepath.Join(CacheDir(cacheRoot, source, firstCommit), "plugins")
	if rc.Root != wantRoot {
		t.Errorf("Root = %q, want %q", rc.Root, wantRoot)
	}
	if len(rc.Manifest.Plugins) != 1 || rc.Manifest.Plugins[0] != "okf" {
		t.Errorf("Manifest.Plugins = %v", rc.Manifest.Plugins)
	}
}

func TestVerifyAndMountPlugin_GitSource_SubdirScoped(t *testing.T) {
	repo, firstCommit := newLocalCatalogGitRepoWithSubdir(t, "plugins")
	cacheRoot := t.TempDir()
	source := "git+https://" + repo
	if _, _, err := fetchGitCatalog(context.Background(), procexec.Default, source, repo, firstCommit, cacheRoot); err != nil {
		t.Fatal(err)
	}

	catalog := ResolvedCatalog{Alias: "official", Source: source, Subdir: "plugins", Manifest: CatalogManifest{Plugins: []string{"okf"}}}
	pluginDir := filepath.Join(CacheDir(cacheRoot, source, firstCommit), "plugins", "okf")
	hash, err := HashTree(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	lock := &Lockfile{Plugins: []PluginLockEntry{{
		ID: "official/okf", Path: "okf", ContentHash: hash, CatalogResolvedRevision: firstCommit,
	}}}

	mounted, err := VerifyAndMountPlugin(catalog, "okf", cacheRoot, lock, testPlectVersion)
	if err != nil {
		t.Fatalf("VerifyAndMountPlugin: unexpected error: %v", err)
	}
	if mounted.Dir != pluginDir {
		t.Errorf("Dir = %q, want %q", mounted.Dir, pluginDir)
	}
}

func TestCheckCatalogNotDrifted_NoLockRecordIsNotDrift(t *testing.T) {
	entry := CatalogEntry{Alias: "local", Source: "path:///x", Subdir: "plugins"}
	if err := CheckCatalogNotDrifted(entry, &Lockfile{}); err != nil {
		t.Errorf("CheckCatalogNotDrifted: unexpected error for a never-locked alias: %v", err)
	}
}

func TestCheckCatalogNotDrifted_MatchingRecordPasses(t *testing.T) {
	entry := CatalogEntry{Alias: "local", Source: "path:///x", Subdir: "plugins"}
	lock := &Lockfile{Catalogs: []CatalogLockRecord{{Alias: "local", CatalogSource: "path:///x", Subdir: "plugins"}}}
	if err := CheckCatalogNotDrifted(entry, lock); err != nil {
		t.Errorf("CheckCatalogNotDrifted: unexpected error for a matching record: %v", err)
	}
}

func TestCheckCatalogNotDrifted_SubdirMismatchFailsLoud(t *testing.T) {
	entry := CatalogEntry{Alias: "local", Source: "path:///x", Subdir: "plugins"}
	lock := &Lockfile{Catalogs: []CatalogLockRecord{{Alias: "local", CatalogSource: "path:///x", Subdir: "other"}}}
	err := CheckCatalogNotDrifted(entry, lock)
	var want *ErrCatalogSourceDrift
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrCatalogSourceDrift", err)
	}
}

func TestCheckCatalogNotDrifted_SourceMismatchFailsLoud(t *testing.T) {
	entry := CatalogEntry{Alias: "local", Source: "path:///x"}
	lock := &Lockfile{Catalogs: []CatalogLockRecord{{Alias: "local", CatalogSource: "path:///y"}}}
	err := CheckCatalogNotDrifted(entry, lock)
	var want *ErrCatalogSourceDrift
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrCatalogSourceDrift", err)
	}
}

func TestVerifyAndMountCatalog_GitSource_Success(t *testing.T) {
	repo, firstCommit, _ := newLocalCatalogGitRepo(t)
	cacheRoot := t.TempDir()
	source := "git+https://" + repo
	if _, _, err := fetchGitCatalog(context.Background(), procexec.Default, source, repo, firstCommit, cacheRoot); err != nil {
		t.Fatal(err)
	}

	entry := CatalogEntry{Alias: "official", Source: source, Plugins: []string{"okf"}}
	lock := &Lockfile{Catalogs: []CatalogLockRecord{{Alias: "official", CatalogSource: source, CatalogResolvedRevision: firstCommit}}}

	rc, err := VerifyAndMountCatalog(entry, lock, cacheRoot)
	if err != nil {
		t.Fatalf("VerifyAndMountCatalog: unexpected error: %v", err)
	}
	if rc.NonReproducible {
		t.Error("NonReproducible = true, want false for a locked git catalog")
	}
	if len(rc.Manifest.Plugins) != 1 || rc.Manifest.Plugins[0] != "okf" {
		t.Errorf("Manifest.Plugins = %v", rc.Manifest.Plugins)
	}
}

func TestVerifyAndMountCatalog_PathEditable_NeedsNoLock(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(dir, "okf"))

	entry := CatalogEntry{Alias: "local", Source: "path+editable://" + dir, Plugins: []string{"okf"}}

	rc, err := VerifyAndMountCatalog(entry, &Lockfile{}, t.TempDir())
	if err != nil {
		t.Fatalf("VerifyAndMountCatalog: unexpected error: %v", err)
	}
	if !rc.NonReproducible {
		t.Error("NonReproducible = false, want true for an editable path catalog")
	}
}

func TestVerifyAndMountCatalog_PathLocked_RequiresCatalogLock(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(dir, "okf"))

	entry := CatalogEntry{Alias: "local", Source: "path://" + dir, Plugins: []string{"okf"}}

	_, err := VerifyAndMountCatalog(entry, &Lockfile{}, t.TempDir())
	var want *ErrMissingCatalogLock
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrMissingCatalogLock", err)
	}
}

func TestVerifyAndMountPlugin_MissingLockFailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(dir, "okf"))
	catalog := ResolvedCatalog{Alias: "local", Source: "path://" + dir, Root: dir, Manifest: CatalogManifest{Plugins: []string{"okf"}}}

	_, err := VerifyAndMountPlugin(catalog, "okf", t.TempDir(), &Lockfile{}, testPlectVersion)
	var want *ErrMissingPluginLock
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrMissingPluginLock", err)
	}
}

func TestVerifyAndMountPlugin_TamperedContentFailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	pluginDir := filepath.Join(dir, "okf")
	writeMinimalPlugin(t, pluginDir)
	hash, err := HashTree(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	catalog := ResolvedCatalog{Alias: "local", Source: "path://" + dir, Root: dir, Manifest: CatalogManifest{Plugins: []string{"okf"}}}
	lock := &Lockfile{Plugins: []PluginLockEntry{{ID: "local/okf", Path: "okf", ContentHash: hash}}}

	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.toml"), []byte("schema_version = 2\nplect_min_version = \"9.9.9\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = VerifyAndMountPlugin(catalog, "okf", t.TempDir(), lock, testPlectVersion)
	var want *ErrHashMismatch
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrHashMismatch", err)
	}
}

func TestVerifyAndMountPlugin_LockedSuccess(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	pluginDir := filepath.Join(dir, "okf")
	writeMinimalPlugin(t, pluginDir)
	hash, err := HashTree(pluginDir)
	if err != nil {
		t.Fatal(err)
	}
	catalog := ResolvedCatalog{Alias: "local", Source: "path://" + dir, Root: dir, Manifest: CatalogManifest{Plugins: []string{"okf"}}}
	lock := &Lockfile{Plugins: []PluginLockEntry{{ID: "local/okf", Path: "okf", ContentHash: hash}}}

	mounted, err := VerifyAndMountPlugin(catalog, "okf", t.TempDir(), lock, testPlectVersion)
	if err != nil {
		t.Fatalf("VerifyAndMountPlugin: unexpected error: %v", err)
	}
	if mounted.ID != "local/okf" || mounted.NonReproducible {
		t.Errorf("Mounted = %+v", mounted)
	}
}

func TestVerifyAndMountPlugin_EditableCatalogNeedsNoLockEntry(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "okf")
	writeMinimalPlugin(t, pluginDir)
	catalog := ResolvedCatalog{Alias: "local", Source: "path+editable://" + dir, Root: dir, NonReproducible: true, Manifest: CatalogManifest{Plugins: []string{"okf"}}}

	mounted, err := VerifyAndMountPlugin(catalog, "okf", t.TempDir(), &Lockfile{}, testPlectVersion)
	if err != nil {
		t.Fatalf("VerifyAndMountPlugin: unexpected error: %v", err)
	}
	if !mounted.NonReproducible {
		t.Error("NonReproducible = false, want true when the owning catalog is editable")
	}
}

func TestVerifyAndMountPlugin_IncompatibleMinVersionFailsLoud(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "okf")
	writeManifest(t, pluginDir, "schema_version = 2\nplect_min_version = \"99.0.0\"\n")
	catalog := ResolvedCatalog{Alias: "local", Source: "path+editable://" + dir, Root: dir, NonReproducible: true, Manifest: CatalogManifest{Plugins: []string{"okf"}}}

	_, err := VerifyAndMountPlugin(catalog, "okf", t.TempDir(), &Lockfile{}, testPlectVersion)
	var want *ErrIncompatible
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrIncompatible", err)
	}
}

func TestVerifyAndMountAll_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\", \"github\"]\n")
	writeMinimalPlugin(t, filepath.Join(dir, "okf"))
	writeMinimalPlugin(t, filepath.Join(dir, "github"))
	okfHash, err := HashTree(filepath.Join(dir, "okf"))
	if err != nil {
		t.Fatal(err)
	}
	githubHash, err := HashTree(filepath.Join(dir, "github"))
	if err != nil {
		t.Fatal(err)
	}

	registrations := &CatalogRegistrations{Catalogs: []CatalogEntry{
		{Alias: "local", Source: "path://" + dir, Plugins: []string{"okf", "github"}},
	}}
	lock := &Lockfile{
		Catalogs: []CatalogLockRecord{{Alias: "local", CatalogSource: "path://" + dir}},
		Plugins: []PluginLockEntry{
			{ID: "local/okf", Path: "okf", ContentHash: okfHash},
			{ID: "local/github", Path: "github", ContentHash: githubHash},
		},
	}

	mounted, err := VerifyAndMountAll(registrations, lock, t.TempDir(), testPlectVersion)
	if err != nil {
		t.Fatalf("VerifyAndMountAll: unexpected error: %v", err)
	}
	if len(mounted) != 2 {
		t.Fatalf("mounted = %+v, want 2 entries", mounted)
	}
	ids := map[string]bool{mounted[0].ID: true, mounted[1].ID: true}
	if !ids["local/okf"] || !ids["local/github"] {
		t.Errorf("ids = %v", ids)
	}
}

func TestVerifyAndMountAll_EnabledPathNotListedInCatalogFailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(dir, "okf"))

	// "github" is enabled in the user's registration but not published by
	// the catalog's own manifest. Left unenabled here so this test isolates
	// that one failure mode rather than also needing a valid "okf" lock
	// entry to get past it first.
	registrations := &CatalogRegistrations{Catalogs: []CatalogEntry{
		{Alias: "local", Source: "path://" + dir, Plugins: []string{"github"}},
	}}
	// A lock entry for "github" is present so the missing-lock-entry check
	// (a different, earlier failure mode) doesn't mask the one under test.
	lock := &Lockfile{
		Catalogs: []CatalogLockRecord{{Alias: "local", CatalogSource: "path://" + dir}},
		Plugins:  []PluginLockEntry{{ID: "local/github", Path: "github", ContentHash: "sha256:whatever"}},
	}

	_, err := VerifyAndMountAll(registrations, lock, t.TempDir(), testPlectVersion)
	var want *ErrPluginNotListed
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want *ErrPluginNotListed", err)
	}
}

// newLocalCatalogGitRepoTwoPlugins creates a git catalog repo with two
// plugins, "okf" and "other". The first commit (tagged v1.0.0) has both at
// baseline content; the second commit changes the content of BOTH plugins,
// so a test can prove which commit a given plugin was actually mounted
// from by comparing content hashes, not just by trusting a returned path.
func newLocalCatalogGitRepoTwoPlugins(t *testing.T) (repoDir, firstCommit, secondCommit string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
	dir := t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=plect-test", "GIT_AUTHOR_EMAIL=plect-test@example.test",
		"GIT_COMMITTER_NAME=plect-test", "GIT_COMMITTER_EMAIL=plect-test@example.test",
	)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(okfVersion, otherVersion string) {
		if err := os.WriteFile(filepath.Join(dir, "catalog.toml"), []byte("schema_version = 2\nplugins = [\"okf\", \"other\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for name, minVersion := range map[string]string{"okf": okfVersion, "other": otherVersion} {
			pluginDir := filepath.Join(dir, name)
			if err := os.MkdirAll(pluginDir, 0o755); err != nil {
				t.Fatal(err)
			}
			content := "schema_version = 2\nplect_min_version = \"" + minVersion + "\"\n"
			if err := os.WriteFile(filepath.Join(pluginDir, "plugin.toml"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	run("init", "--quiet", "--initial-branch=main")
	write("0.1.0", "0.1.0")
	run("add", ".")
	run("commit", "--quiet", "-m", "v1")
	run("tag", "v1.0.0")
	first := run("rev-parse", "HEAD")

	write("0.2.0", "0.2.0")
	run("commit", "--quiet", "-am", "v2")
	second := run("rev-parse", "HEAD")

	return dir, first, second
}

// TestVerifyAndMountAll_GitSiblingsStayIndependentlyPinnedAcrossPartialUpdate
// is the regression test for the bug a reviewer caught: `plugin update`
// repoints only the target plugin's lock entry (and bumps the shared
// catalog lock record so a later `plugin add` reuses the fresher snapshot),
// but a sibling plugin's OWN lock entry can still point at an older commit.
// Mounting must honor each plugin's own locked commit, never the catalog's
// shared lock record — otherwise a sibling silently mounts (and, as here,
// fails to verify) content from a commit it was never pinned to.
func TestVerifyAndMountAll_GitSiblingsStayIndependentlyPinnedAcrossPartialUpdate(t *testing.T) {
	repo, firstCommit, secondCommit := newLocalCatalogGitRepoTwoPlugins(t)
	cacheRoot := t.TempDir()
	source := "git+https://" + repo

	rootA, _, err := fetchGitCatalog(context.Background(), procexec.Default, source, repo, firstCommit, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	otherHashA, err := HashTree(filepath.Join(rootA, "other"))
	if err != nil {
		t.Fatal(err)
	}

	rootB, _, err := fetchGitCatalog(context.Background(), procexec.Default, source, repo, secondCommit, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	okfHashB, err := HashTree(filepath.Join(rootB, "okf"))
	if err != nil {
		t.Fatal(err)
	}

	// Simulates: both plugins were added at firstCommit, then only
	// "local/okf" was updated to secondCommit — the shared catalog lock
	// record now points at secondCommit (as CatalogUpdate/PluginUpdate
	// leaves it), but "local/other"'s own lock entry still pins firstCommit.
	registrations := &CatalogRegistrations{Catalogs: []CatalogEntry{
		{Alias: "local", Source: source, Plugins: []string{"okf", "other"}},
	}}
	lock := &Lockfile{
		Catalogs: []CatalogLockRecord{{Alias: "local", CatalogSource: source, CatalogResolvedRevision: secondCommit}},
		Plugins: []PluginLockEntry{
			{ID: "local/okf", CatalogAlias: "local", CatalogSource: source, CatalogResolvedRevision: secondCommit, Path: "okf", ContentHash: okfHashB},
			{ID: "local/other", CatalogAlias: "local", CatalogSource: source, CatalogResolvedRevision: firstCommit, Path: "other", ContentHash: otherHashA},
		},
	}

	mounted, err := VerifyAndMountAll(registrations, lock, cacheRoot, testPlectVersion)
	if err != nil {
		t.Fatalf("VerifyAndMountAll: unexpected error: %v", err)
	}
	byID := make(map[string]Mounted, len(mounted))
	for _, m := range mounted {
		byID[m.ID] = m
	}
	if !strings.HasPrefix(byID["local/okf"].Dir, rootB) {
		t.Errorf("local/okf.Dir = %q, want under the secondCommit snapshot %q", byID["local/okf"].Dir, rootB)
	}
	if !strings.HasPrefix(byID["local/other"].Dir, rootA) {
		t.Errorf("local/other.Dir = %q, want under the firstCommit snapshot %q (its own pinned commit)", byID["local/other"].Dir, rootA)
	}
}

// newLocalCatalogGitRepoPluginRemovedLater creates a git catalog repo where
// the first commit (tagged v1.0.0) lists two plugins, "okf" and "other",
// and the second commit removes "other" from catalog.toml's `plugins` list
// (and deletes its directory, so the tree stays consistent with a strict
// "no unlisted plugin.toml" catalog — the shape a catalog author would
// actually publish when dropping a plugin).
func newLocalCatalogGitRepoPluginRemovedLater(t *testing.T) (repoDir, firstCommit, secondCommit string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
	dir := t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=plect-test", "GIT_AUTHOR_EMAIL=plect-test@example.test",
		"GIT_COMMITTER_NAME=plect-test", "GIT_COMMITTER_EMAIL=plect-test@example.test",
	)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "catalog.toml"), []byte("schema_version = 2\nplugins = [\"okf\", \"other\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"okf", "other"} {
		pluginDir := filepath.Join(dir, name)
		if err := os.MkdirAll(pluginDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pluginDir, "plugin.toml"), []byte("schema_version = 2\nplect_min_version = \"0.1.0\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "v1")
	run("tag", "v1.0.0")
	first := run("rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "catalog.toml"), []byte("schema_version = 2\nplugins = [\"okf\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "okf", "plugin.toml"), []byte("schema_version = 2\nplect_min_version = \"0.2.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("rm", "--quiet", "-r", "other")
	run("add", ".")
	run("commit", "--quiet", "-m", "v2: drop other")
	second := run("rev-parse", "HEAD")

	return dir, first, second
}

// TestVerifyAndMountAll_GitSiblingRemovedFromNewerManifestStaysMountable is
// the regression test for the follow-up finding: membership in
// catalog.toml's `plugins` list must be checked against each plugin's OWN
// locked commit, not the catalog's current shared snapshot. A catalog
// author dropping "other" in a newer release must not break an untouched
// sibling still validly pinned to the older commit that published it.
func TestVerifyAndMountAll_GitSiblingRemovedFromNewerManifestStaysMountable(t *testing.T) {
	repo, firstCommit, secondCommit := newLocalCatalogGitRepoPluginRemovedLater(t)
	cacheRoot := t.TempDir()
	source := "git+https://" + repo

	rootA, _, err := fetchGitCatalog(context.Background(), procexec.Default, source, repo, firstCommit, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	otherHashA, err := HashTree(filepath.Join(rootA, "other"))
	if err != nil {
		t.Fatal(err)
	}

	rootB, _, err := fetchGitCatalog(context.Background(), procexec.Default, source, repo, secondCommit, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	okfHashB, err := HashTree(filepath.Join(rootB, "okf"))
	if err != nil {
		t.Fatal(err)
	}

	// Simulates: both plugins added at firstCommit, then "local/okf" was
	// updated to secondCommit (which no longer publishes "other" at all).
	// The shared catalog lock record now points at secondCommit, but
	// "local/other"'s own lock entry is untouched, still pinning firstCommit
	// — where it was, and still is, validly published.
	registrations := &CatalogRegistrations{Catalogs: []CatalogEntry{
		{Alias: "local", Source: source, Plugins: []string{"okf", "other"}},
	}}
	lock := &Lockfile{
		Catalogs: []CatalogLockRecord{{Alias: "local", CatalogSource: source, CatalogResolvedRevision: secondCommit}},
		Plugins: []PluginLockEntry{
			{ID: "local/okf", CatalogAlias: "local", CatalogSource: source, CatalogResolvedRevision: secondCommit, Path: "okf", ContentHash: okfHashB},
			{ID: "local/other", CatalogAlias: "local", CatalogSource: source, CatalogResolvedRevision: firstCommit, Path: "other", ContentHash: otherHashA},
		},
	}

	mounted, err := VerifyAndMountAll(registrations, lock, cacheRoot, testPlectVersion)
	if err != nil {
		t.Fatalf("VerifyAndMountAll: unexpected error: %v (a sibling still valid at its own locked commit must not be rejected because a newer commit dropped it)", err)
	}
	if len(mounted) != 2 {
		t.Fatalf("mounted = %+v, want 2 entries", mounted)
	}
}
