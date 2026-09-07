package plugins

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/procexec"
	"github.com/kecbigmt/plecture/app/internal/testpath"
)

// newLocalCatalogGitRepo creates a git repository at a local path shaped as
// a valid catalog (catalog.toml listing one plugin "okf"), with one commit
// tagged v1.0.0, then a second commit that bumps the plugin's
// plect_min_version. Cloning a local path this way exercises the real git
// plumbing without a network dependency.
func newLocalCatalogGitRepo(t *testing.T) (repoDir, firstCommit, secondCommit string) {
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
	writeCatalogFixture := func(minVersion string) {
		if err := os.WriteFile(filepath.Join(dir, "catalog.toml"), []byte("schema_version = 2\nplugins = [\"okf\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "okf"), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "schema_version = 2\nplect_min_version = \"" + minVersion + "\"\n"
		if err := os.WriteFile(filepath.Join(dir, "okf", "plugin.toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "--quiet", "--initial-branch=main")
	writeCatalogFixture("0.1.0")
	run("add", ".")
	run("commit", "--quiet", "-m", "v1")
	run("tag", "v1.0.0")
	first := run("rev-parse", "HEAD")

	writeCatalogFixture("0.2.0")
	run("commit", "--quiet", "-am", "v2")
	second := run("rev-parse", "HEAD")

	return dir, first, second
}

func TestFetchCatalog_GitSource(t *testing.T) {
	repo, firstCommit, _ := newLocalCatalogGitRepo(t)
	cacheRoot := t.TempDir()

	root, resolvedRevision, err := fetchGitCatalog(context.Background(), procexec.Default, "git+https://"+repo, repo, "v1.0.0", cacheRoot)
	if err != nil {
		t.Fatalf("fetchGitCatalog: unexpected error: %v", err)
	}
	if resolvedRevision != firstCommit {
		t.Errorf("resolvedRevision = %q, want %q", resolvedRevision, firstCommit)
	}
	wantRoot := CacheDir(cacheRoot, "git+https://"+repo, firstCommit)
	if root != wantRoot {
		t.Errorf("root = %q, want %q", root, wantRoot)
	}
	if _, err := os.Stat(filepath.Join(root, "catalog.toml")); err != nil {
		t.Errorf("catalog.toml not checked out: %v", err)
	}
}

func TestFetchCatalog_GitSource_ReusesCacheForIdenticalSnapshot(t *testing.T) {
	repo, firstCommit, _ := newLocalCatalogGitRepo(t)
	cacheRoot := t.TempDir()

	root1, _, err := fetchGitCatalog(context.Background(), procexec.Default, "git+https://"+repo, repo, firstCommit, cacheRoot)
	if err != nil {
		t.Fatalf("first fetchGitCatalog: unexpected error: %v", err)
	}
	root2, _, err := fetchGitCatalog(context.Background(), procexec.Default, "git+https://"+repo, repo, firstCommit, cacheRoot)
	if err != nil {
		t.Fatalf("second fetchGitCatalog: unexpected error: %v", err)
	}
	if root1 != root2 {
		t.Errorf("second fetch of the same snapshot landed at a different path: %q vs %q", root1, root2)
	}
}

func TestFetchCatalog_PathSource(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(dir, "okf"))

	result, err := FetchCatalog(context.Background(), procexec.Default, "path://"+dir, "", "", t.TempDir())
	if err != nil {
		t.Fatalf("FetchCatalog: unexpected error: %v", err)
	}
	if len(result.Manifest.Plugins) != 1 || result.Manifest.Plugins[0] != "okf" {
		t.Errorf("Manifest.Plugins = %v", result.Manifest.Plugins)
	}
	if result.ResolvedRevision != "" {
		t.Errorf("ResolvedRevision = %q, want empty for a path source", result.ResolvedRevision)
	}
}

func TestFetchCatalog_PathSource_Subdir(t *testing.T) {
	root := testpath.Real(t, t.TempDir())
	writeCatalogManifest(t, filepath.Join(root, "plugins"), "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(root, "plugins", "okf"))

	result, err := FetchCatalog(context.Background(), procexec.Default, "path://"+root, "", "plugins", t.TempDir())
	if err != nil {
		t.Fatalf("FetchCatalog: unexpected error: %v", err)
	}
	wantRoot := filepath.Join(root, "plugins")
	if result.Root != wantRoot {
		t.Errorf("Root = %q, want %q", result.Root, wantRoot)
	}
	if len(result.Manifest.Plugins) != 1 || result.Manifest.Plugins[0] != "okf" {
		t.Errorf("Manifest.Plugins = %v", result.Manifest.Plugins)
	}
}

func TestFetchCatalog_Subdir_EscapeRejected(t *testing.T) {
	root := t.TempDir()
	writeCatalogManifest(t, root, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(root, "okf"))

	if _, err := FetchCatalog(context.Background(), procexec.Default, "path://"+root, "", "../escape", t.TempDir()); err == nil {
		t.Fatal("want error for a subdir that escapes the source root, got nil")
	}
}

func TestFetchCatalog_PathEditableSource(t *testing.T) {
	dir := t.TempDir()
	writeCatalogManifest(t, dir, "schema_version = 2\nplugins = [\"okf\"]\n")
	writeMinimalPlugin(t, filepath.Join(dir, "okf"))

	result, err := FetchCatalog(context.Background(), procexec.Default, "path+editable://"+dir, "", "", t.TempDir())
	if err != nil {
		t.Fatalf("FetchCatalog: unexpected error: %v", err)
	}
	if len(result.Manifest.Plugins) != 1 {
		t.Errorf("Manifest.Plugins = %v", result.Manifest.Plugins)
	}
}

func TestFetchCatalog_GitSource_MissingRevision(t *testing.T) {
	repo, _, _ := newLocalCatalogGitRepo(t)

	if _, err := FetchCatalog(context.Background(), procexec.Default, "git+https://"+repo, "", "", t.TempDir()); err == nil {
		t.Fatal("want error when a git source has no revision, got nil")
	}
}

func TestFetchCatalog_UnsupportedScheme(t *testing.T) {
	if _, err := FetchCatalog(context.Background(), procexec.Default, "archive+https://example.com/catalog.tar.gz", "", "", t.TempDir()); err == nil {
		t.Fatal("want error for an unsupported scheme, got nil")
	}
}

func TestFetchCatalog_InvalidCatalogManifestFailsLoud(t *testing.T) {
	dir := t.TempDir() // no catalog.toml

	if _, err := FetchCatalog(context.Background(), procexec.Default, "path://"+dir, "", "", t.TempDir()); err == nil {
		t.Fatal("want error when the source has no catalog.toml, got nil")
	}
}

func TestCacheDir(t *testing.T) {
	got := CacheDir("/root", "git+https://example.com/x", "abcd1234")
	want := filepath.Join("/root", SourceDigest("git+https://example.com/x"), "abcd1234")
	if got != want {
		t.Errorf("CacheDir = %q, want %q", got, want)
	}
}

func TestSourceDigest_DiffersByExactSource(t *testing.T) {
	a := SourceDigest("git+https://example.com/x")
	b := SourceDigest("git+https://example.com/y")
	if a == b {
		t.Error("SourceDigest must differ for different sources")
	}
	if a != SourceDigest("git+https://example.com/x") {
		t.Error("SourceDigest must be deterministic for the same source")
	}
}
