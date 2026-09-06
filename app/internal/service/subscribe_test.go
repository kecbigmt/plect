package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
)

// writeSubscribeProvider drops a provider with a resolver + a subscribe hook
// that records its rendered SessionName/ResourceID to recordPath, so a test
// can assert what core forwarded.
func writeSubscribeProvider(t *testing.T, id, match, recordPath string) *config.Config {
	t.Helper()
	baseDir := t.TempDir()
	providersDir := filepath.Join(baseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`
[%[1]s]
kind  = "workspace_provider"
match = %[2]q
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[%[1]s.setup]
type    = "exec"
command = "printf"
args    = ['{"workspace_dir":"/tmp/x"}']

[%[1]s.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%%s\n%%s\n" "$1" "$2" > "$3"', "provider",
  { from = "session.name" }, { from = "resource.id" }, %[3]q]
`, id, match, recordPath)
	if err := os.WriteFile(filepath.Join(providersDir, id+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &config.Config{BaseDir: baseDir}
}

// subscribeStore returns a store pre-seeded with the given session names so
// the existence guard in Subscribe passes.
func subscribeStore(t *testing.T, names ...string) *state.Store {
	t.Helper()
	store := state.NewStore(t.TempDir())
	now := time.Now()
	for _, n := range names {
		if err := store.Put(&domain.Session{Name: n, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	return store
}

const ghMatch = `^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(issues|pull)/(?P<number>\d+)`

func TestSubscribe_RunsProviderHookWithEnv(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeSubscribeProvider(t, "github", ghMatch, rec)
	store := subscribeStore(t, "org/repo-7")

	err := Subscribe(cfg, store, SubscribeParams{
		ResourceID:  "https://github.com/org/repo/pull/7",
		SessionName: "org/repo-7",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got, readErr := os.ReadFile(rec)
	if readErr != nil {
		t.Fatalf("read record: %v", readErr)
	}
	want := "org/repo-7\nhttps://github.com/org/repo/pull/7\n"
	if string(got) != want {
		t.Errorf("hook recorded %q, want %q", got, want)
	}
}

// Subscribe forwards the session's own workspace branch into the hook
// context, so a workspace provider's subscribe hook can key its own delivery
// mechanism on it (e.g. the GitHub watcher's linked-PR discovery for an
// issue resource) without any core knowledge of what the branch is for.
func TestSubscribe_ForwardsSessionBranch(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	baseDir := t.TempDir()
	providersDir := filepath.Join(baseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`
[github]
kind  = "workspace_provider"
match = %q
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[github.setup]
type    = "exec"
command = "printf"
args    = ['{"workspace_dir":"/tmp/x"}']

[github.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%%s" "$1" > "$2"', "provider", { from = "session.branch" }, %q]
`, ghMatch, rec)
	if err := os.WriteFile(filepath.Join(providersDir, "github.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseDir: baseDir}
	store := state.NewStore(t.TempDir())
	now := time.Now()
	if err := store.Put(&domain.Session{Name: "org/repo-7", Branch: "issue/7", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if err := Subscribe(cfg, store, SubscribeParams{
		ResourceID:  "https://github.com/org/repo/pull/7",
		SessionName: "org/repo-7",
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got, readErr := os.ReadFile(rec)
	if readErr != nil {
		t.Fatalf("read record: %v", readErr)
	}
	if string(got) != "issue/7" {
		t.Errorf("subscribe hook saw session.branch = %q, want issue/7", got)
	}
}

func TestSubscribe_DefaultsSessionFromEnv(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeSubscribeProvider(t, "github", ghMatch, rec)
	store := subscribeStore(t, "env/session-9")
	t.Setenv("PLECT_SESSION_NAME", "env/session-9")

	err := Subscribe(cfg, store, SubscribeParams{ResourceID: "https://github.com/org/repo/issues/9"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got, _ := os.ReadFile(rec)
	want := "env/session-9\nhttps://github.com/org/repo/issues/9\n"
	if string(got) != want {
		t.Errorf("hook recorded %q, want %q", got, want)
	}
}

func TestSubscribe_ParamSessionOverridesEnv(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeSubscribeProvider(t, "github", ghMatch, rec)
	store := subscribeStore(t, "flag/session")
	t.Setenv("PLECT_SESSION_NAME", "env/session")

	if err := Subscribe(cfg, store, SubscribeParams{
		ResourceID:  "https://github.com/org/repo/pull/1",
		SessionName: "flag/session",
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got, _ := os.ReadFile(rec)
	if !strings.HasPrefix(string(got), "flag/session\n") {
		t.Errorf("expected flag session to win, got %q", got)
	}
}

func TestSubscribe_NoSessionInScope(t *testing.T) {
	cfg := writeSubscribeProvider(t, "github", ghMatch, filepath.Join(t.TempDir(), "rec"))
	t.Setenv("PLECT_SESSION_NAME", "")

	err := Subscribe(cfg, subscribeStore(t), SubscribeParams{ResourceID: "https://github.com/org/repo/pull/1"})
	assertErrCode(t, err, ErrInvalidInput)
}

func TestSubscribe_GhostSessionRejected(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeSubscribeProvider(t, "github", ghMatch, rec)
	// store has no such session — an explicit --session typo must be rejected
	// before the hook runs (no ghost subscription).
	err := Subscribe(cfg, subscribeStore(t), SubscribeParams{
		ResourceID:  "https://github.com/org/repo/pull/1",
		SessionName: "typo/sess-1",
	})
	assertErrCode(t, err, ErrSessionNotFound)
	if _, statErr := os.Stat(rec); statErr == nil {
		t.Error("subscribe hook ran for a non-existent session")
	}
}

func TestSubscribe_EmptyResource(t *testing.T) {
	cfg := writeSubscribeProvider(t, "github", ghMatch, filepath.Join(t.TempDir(), "rec"))
	err := Subscribe(cfg, subscribeStore(t, "s"), SubscribeParams{ResourceID: "", SessionName: "s"})
	assertErrCode(t, err, ErrInvalidInput)
}

func TestSubscribe_NoProviderMatches(t *testing.T) {
	cfg := writeSubscribeProvider(t, "github", ghMatch, filepath.Join(t.TempDir(), "rec"))
	err := Subscribe(cfg, subscribeStore(t, "s"), SubscribeParams{
		ResourceID:  "https://jira.example.com/browse/PROJ-1",
		SessionName: "s",
	})
	assertErrCode(t, err, ErrInvalidInput)
}

func TestSubscribe_ProviderWithoutSubscribeHook(t *testing.T) {
	baseDir := t.TempDir()
	providersDir := filepath.Join(baseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`
[github]
kind  = "workspace_provider"
match = %q
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[github.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']
`, ghMatch)
	if err := os.WriteFile(filepath.Join(providersDir, "github.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseDir: baseDir}

	err := Subscribe(cfg, subscribeStore(t, "s"), SubscribeParams{
		ResourceID:  "https://github.com/org/repo/pull/1",
		SessionName: "s",
	})
	assertErrCode(t, err, ErrInvalidInput)
	if err != nil && !strings.Contains(err.Error(), "does not support subscribe") {
		t.Errorf("error = %q, want it to mention subscribe support", err)
	}
}

func TestSubscribe_HookFailureSurfacesStderr(t *testing.T) {
	baseDir := t.TempDir()
	providersDir := filepath.Join(baseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`
[github]
kind  = "workspace_provider"
match = %q
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[github.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[github.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 3"]
`, ghMatch)
	if err := os.WriteFile(filepath.Join(providersDir, "github.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BaseDir: baseDir}

	err := Subscribe(cfg, subscribeStore(t, "s"), SubscribeParams{
		ResourceID:  "https://github.com/org/repo/pull/1",
		SessionName: "s",
	})
	assertErrCode(t, err, ErrExecutionFailed)
	if err != nil && !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to include hook stderr", err)
	}
}

func assertErrCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", code)
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error %v is not *service.Error", err)
	}
	if svcErr.Code != code {
		t.Errorf("error code = %q, want %q", svcErr.Code, code)
	}
}
