package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
)

// fixtureResourceMatch is a workspace-provider resolver for these tests'
// fake resources — the same four-segment shape a real workspace provider
// resolves, but under a scheme no real provider claims, since none of
// subscribeIfWired/unsubscribeIfWired's own logic is provider-specific: it
// only needs a resolver that matches or doesn't, not a real one.
const fixtureResourceMatch = `^resource://(?P<owner>[^/]+)/(?P<repo>[^/]+)/(issues|pull)/(?P<number>\d+)`

func TestSubscribeIfWired_EmptyResourceIsANoOp(t *testing.T) {
	cfg := &config.Config{BaseDir: t.TempDir()}
	wired, err := subscribeIfWired(cfg, "s", "", "")
	if err != nil || wired {
		t.Fatalf("wired=%v err=%v, want false, nil", wired, err)
	}
}

func TestSubscribeIfWired_NoProviderMatchIsSilentlySkipped(t *testing.T) {
	cfg := &config.Config{BaseDir: t.TempDir()}
	wired, err := subscribeIfWired(cfg, "s", "opaque-resource-no-provider-recognizes", "")
	if err != nil {
		t.Fatalf("subscribeIfWired: %v", err)
	}
	if wired {
		t.Error("a resource no workspace provider matches must not be reported as wired")
	}
}

func TestSubscribeIfWired_ProviderWithoutSubscribeHookIsSilentlySkipped(t *testing.T) {
	baseDir := t.TempDir()
	writeProviderDoc(t, baseDir, "fixture", fixtureResourceMatch, "")
	cfg := &config.Config{BaseDir: baseDir}

	wired, err := subscribeIfWired(cfg, "s", "resource://org/repo/pull/1", "")
	if err != nil {
		t.Fatalf("subscribeIfWired: %v", err)
	}
	if wired {
		t.Error("a provider without a subscribe hook must not be reported as wired")
	}
}

func TestSubscribeIfWired_RunsSubscribeHookAndReportsWired(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeSubscribeProvider(t, "fixture", fixtureResourceMatch, rec)

	wired, err := subscribeIfWired(cfg, "sess-7", "resource://org/repo/pull/7", "")
	if err != nil {
		t.Fatalf("subscribeIfWired: %v", err)
	}
	if !wired {
		t.Error("a matched provider with a subscribe hook must be reported as wired")
	}
	got, readErr := os.ReadFile(rec)
	if readErr != nil {
		t.Fatalf("read record: %v", readErr)
	}
	want := "sess-7\nresource://org/repo/pull/7\n"
	if string(got) != want {
		t.Errorf("hook recorded %q, want %q", got, want)
	}
}

func TestSubscribeIfWired_IdempotentOnRepeatedCall(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeSubscribeProvider(t, "fixture", fixtureResourceMatch, rec)

	for i := 0; i < 2; i++ {
		if _, err := subscribeIfWired(cfg, "sess-7", "resource://org/repo/pull/7", ""); err != nil {
			t.Fatalf("subscribeIfWired[%d]: %v", i, err)
		}
	}
}

func TestSubscribeIfWired_AmbiguousMatchIsAnError(t *testing.T) {
	baseDir := t.TempDir()
	writeProviderDoc(t, baseDir, "a", fixtureResourceMatch, "")
	writeProviderDoc(t, baseDir, "b", fixtureResourceMatch, "")
	cfg := &config.Config{BaseDir: baseDir}

	_, err := subscribeIfWired(cfg, "s", "resource://org/repo/pull/1", "")
	assertErrCode(t, err, ErrInvalidInput)
}

func TestSubscribeIfWired_HookFailureSurfacesStderr(t *testing.T) {
	baseDir := t.TempDir()
	body := `
[fixture]
kind  = "workspace_provider"
match = '` + fixtureResourceMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[fixture.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[fixture.health.alive]
type = "noop"

[fixture.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 3"]
`
	writeFileService(t, filepath.Join(baseDir, "workspaces", "fixture.toml"), body)
	cfg := &config.Config{BaseDir: baseDir}

	_, err := subscribeIfWired(cfg, "s", "resource://org/repo/pull/1", "")
	assertErrCode(t, err, ErrExecutionFailed)
}

func TestUnsubscribeIfWired_EmptyResourceIsANoOp(t *testing.T) {
	cfg := &config.Config{BaseDir: t.TempDir()}
	unsubscribed, err := unsubscribeIfWired(cfg, "s", "")
	if err != nil || unsubscribed {
		t.Fatalf("unsubscribed=%v err=%v, want false, nil", unsubscribed, err)
	}
}

func TestUnsubscribeIfWired_NoProviderMatchIsSilentlySkipped(t *testing.T) {
	cfg := &config.Config{BaseDir: t.TempDir()}
	unsubscribed, err := unsubscribeIfWired(cfg, "s", "opaque-resource-no-provider-recognizes")
	if err != nil || unsubscribed {
		t.Fatalf("unsubscribed=%v err=%v, want false, nil", unsubscribed, err)
	}
}

func TestUnsubscribeIfWired_ProviderWithoutUnsubscribeHookIsSilentlySkipped(t *testing.T) {
	baseDir := t.TempDir()
	writeProviderDoc(t, baseDir, "fixture", fixtureResourceMatch, "")
	cfg := &config.Config{BaseDir: baseDir}

	unsubscribed, err := unsubscribeIfWired(cfg, "s", "resource://org/repo/pull/1")
	if err != nil || unsubscribed {
		t.Fatalf("unsubscribed=%v err=%v, want false, nil", unsubscribed, err)
	}
}

func TestUnsubscribeIfWired_RunsUnsubscribeHook(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeSubscribeUnsubscribeProvider(t, "fixture", fixtureResourceMatch, "", rec)

	unsubscribed, err := unsubscribeIfWired(cfg, "sess-7", "resource://org/repo/pull/7")
	if err != nil {
		t.Fatalf("unsubscribeIfWired: %v", err)
	}
	if !unsubscribed {
		t.Error("a matched provider with an unsubscribe hook must be reported as unsubscribed")
	}
	got, readErr := os.ReadFile(rec)
	if readErr != nil {
		t.Fatalf("read record: %v", readErr)
	}
	want := "sess-7\nresource://org/repo/pull/7\n"
	if string(got) != want {
		t.Errorf("hook recorded %q, want %q", got, want)
	}
}

func TestUnsubscribeIfWired_AmbiguousMatchIsSilentlySkipped(t *testing.T) {
	baseDir := t.TempDir()
	writeProviderDoc(t, baseDir, "a", fixtureResourceMatch, "")
	writeProviderDoc(t, baseDir, "b", fixtureResourceMatch, "")
	cfg := &config.Config{BaseDir: baseDir}

	unsubscribed, err := unsubscribeIfWired(cfg, "s", "resource://org/repo/pull/1")
	if err != nil || unsubscribed {
		t.Fatalf("unsubscribed=%v err=%v, want false, nil", unsubscribed, err)
	}
}

func TestUnsubscribeIfWired_HookFailureSurfacesStderr(t *testing.T) {
	baseDir := t.TempDir()
	body := `
[fixture]
kind  = "workspace_provider"
match = '` + fixtureResourceMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[fixture.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[fixture.health.alive]
type = "noop"

[fixture.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 3"]
`
	writeFileService(t, filepath.Join(baseDir, "workspaces", "fixture.toml"), body)
	cfg := &config.Config{BaseDir: baseDir}

	unsubscribed, err := unsubscribeIfWired(cfg, "s", "resource://org/repo/pull/1")
	if unsubscribed {
		t.Error("a failed hook must not be reported as unsubscribed")
	}
	assertErrCode(t, err, ErrExecutionFailed)
}

// writeProviderDoc writes a minimal workspace provider with a resolver and a
// no-op setup, plus the given extra table text (e.g. a subscribe hook),
// verbatim.
func writeProviderDoc(t *testing.T, baseDir, id, match, extra string) {
	t.Helper()
	body := `
[` + id + `]
kind  = "workspace_provider"
match = '` + match + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[` + id + `.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[` + id + `.health.alive]
type = "noop"
` + extra
	writeFileService(t, filepath.Join(baseDir, "workspaces", id+".toml"), body)
}

// writeSubscribeUnsubscribeProvider drops a provider with a resolver plus
// subscribe and unsubscribe hooks; subRecordPath/unsubRecordPath ("" to skip)
// record the rendered SessionName/ResourceID each hook ran with.
func writeSubscribeUnsubscribeProvider(t *testing.T, id, match, subRecordPath, unsubRecordPath string) *config.Config {
	t.Helper()
	baseDir := t.TempDir()
	var extra string
	if subRecordPath != "" {
		extra += `
[` + id + `.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%s\n%s\n" "$1" "$2" > "$3"', "provider",
  { from = "session.name" }, { from = "resource.id" }, "` + subRecordPath + `"]
`
	}
	if unsubRecordPath != "" {
		extra += `
[` + id + `.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%s\n%s\n" "$1" "$2" > "$3"', "provider",
  { from = "session.name" }, { from = "resource.id" }, "` + unsubRecordPath + `"]
`
	}
	writeProviderDoc(t, baseDir, id, match, extra)
	return &config.Config{BaseDir: baseDir}
}

func writeFileService(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
