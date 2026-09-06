package effect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plugins"
)

func mustMount(id, dir string, executables ...plugins.Executable) plugins.Mounted {
	return plugins.Mounted{ID: id, Dir: dir, Manifest: plugins.Manifest{Executables: executables}}
}

func TestRunProviderSubscribe_ReadsItsSurfaceRoots(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "subscribe.out")
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Subscribe: &lang.Action{
			Type:    lang.ActionExec,
			Command: "sh",
			Args: []*lang.Value{
				{Form: lang.FormLiteral, Literal: "-c"},
				{Form: lang.FormLiteral, Literal: `printf '%s %s' "$1" "$2" > "$3"`},
				{Form: lang.FormLiteral, Literal: "subscribe"},
				{Form: lang.FormFrom, From: "resource.id"},
				{Form: lang.FormFrom, From: "session.name"},
				{Form: lang.FormLiteral, Literal: marker},
			},
		},
	}
	if err := RunWorkspaceProviderSubscribe(prov, SubscribeHookVars{ResourceID: "res-1", SessionName: "sess-1"}); err != nil {
		t.Fatalf("RunWorkspaceProviderSubscribe: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "res-1 sess-1" {
		t.Errorf("subscribe saw %q, want the resource and session", got)
	}
}

// A subscribe hook that reads session.branch sees the session's workspace
// branch, so the GitHub workspace provider (and any other one keyed on a
// session's working branch) can forward it to its own delivery mechanism.
func TestRunProviderSubscribe_ProjectsSessionBranch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "branch.out")
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Subscribe: &lang.Action{
			Type:    lang.ActionExec,
			Command: "sh",
			Args: []*lang.Value{
				{Form: lang.FormLiteral, Literal: "-c"},
				{Form: lang.FormLiteral, Literal: `printf '%s' "$1" > "$2"`},
				{Form: lang.FormLiteral, Literal: "subscribe"},
				{Form: lang.FormFrom, From: "session.branch"},
				{Form: lang.FormLiteral, Literal: marker},
			},
		},
	}
	if err := RunWorkspaceProviderSubscribe(prov, SubscribeHookVars{ResourceID: "res-1", SessionName: "sess-1", Branch: "issue/1"}); err != nil {
		t.Fatalf("RunWorkspaceProviderSubscribe: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "issue/1" {
		t.Errorf("session.branch = %q, want issue/1", got)
	}
}

// A provider that produced no branch (or has no use for one) must see
// subscribe behave exactly as before Branch existed: session.branch resolves
// to the empty string rather than failing the projection.
func TestRunProviderSubscribe_EmptyBranchResolvesToEmptyString(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "branch.out")
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Subscribe: &lang.Action{
			Type:    lang.ActionExec,
			Command: "sh",
			Args: []*lang.Value{
				{Form: lang.FormLiteral, Literal: "-c"},
				{Form: lang.FormLiteral, Literal: `printf '[%s]' "$1" > "$2"`},
				{Form: lang.FormLiteral, Literal: "subscribe"},
				{Form: lang.FormFrom, From: "session.branch"},
				{Form: lang.FormLiteral, Literal: marker},
			},
		},
	}
	if err := RunWorkspaceProviderSubscribe(prov, SubscribeHookVars{ResourceID: "res-1", SessionName: "sess-1"}); err != nil {
		t.Fatalf("RunWorkspaceProviderSubscribe: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "[]" {
		t.Errorf("session.branch = %q, want empty", got)
	}
}

func TestRunProviderSubscribe_NoSubscribeHookIsAnError(t *testing.T) {
	err := RunWorkspaceProviderSubscribe(config.WorkspaceProviderConfig{ID: "wf"}, SubscribeHookVars{ResourceID: "r", SessionName: "s"})
	if err == nil {
		t.Fatal("expected an error when the provider declares no subscribe hook")
	}
}

// A subscribe hook reaches its own plugin's executables through `bin` — it
// otherwise has only bare command names on PATH available.
func TestRunProviderSubscribe_ResolvesBinReference(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "bin.out")
	mounted := []plugins.Mounted{mustMount("official/github", "/mnt/github",
		plugins.Executable{Name: "github-watcher", Path: "bin/github-watcher"})}
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Subscribe: &lang.Action{
			Type:    lang.ActionExec,
			Command: "sh",
			Args: []*lang.Value{
				{Form: lang.FormLiteral, Literal: "-c"},
				{Form: lang.FormLiteral, Literal: `printf '%s' "$1" > "$2"`},
				{Form: lang.FormLiteral, Literal: "subscribe"},
				{Form: lang.FormBin, Bin: "official/github/github-watcher"},
				{Form: lang.FormLiteral, Literal: marker},
			},
		},
	}
	if err := RunWorkspaceProviderSubscribe(prov, SubscribeHookVars{ResourceID: "r", SessionName: "s", Plugins: mounted}); err != nil {
		t.Fatalf("RunWorkspaceProviderSubscribe: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/mnt/github", "bin", "github-watcher")
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Errorf("resolved executable = %q, want %q", got, want)
	}
}

func TestRunProviderUnsubscribe_ReadsItsSurfaceRoots(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unsubscribe.out")
	prov := config.WorkspaceProviderConfig{
		ID: "wf",
		Unsubscribe: &lang.Action{
			Type:    lang.ActionExec,
			Command: "sh",
			Args: []*lang.Value{
				{Form: lang.FormLiteral, Literal: "-c"},
				{Form: lang.FormLiteral, Literal: `printf '%s %s' "$1" "$2" > "$3"`},
				{Form: lang.FormLiteral, Literal: "unsubscribe"},
				{Form: lang.FormFrom, From: "resource.id"},
				{Form: lang.FormFrom, From: "session.name"},
				{Form: lang.FormLiteral, Literal: marker},
			},
		},
	}
	if err := RunWorkspaceProviderUnsubscribe(prov, SubscribeHookVars{ResourceID: "res-1", SessionName: "sess-1"}); err != nil {
		t.Fatalf("RunWorkspaceProviderUnsubscribe: %v", err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "res-1 sess-1" {
		t.Errorf("unsubscribe saw %q, want the resource and session", got)
	}
}

func TestRunProviderUnsubscribe_NoUnsubscribeHookIsAnError(t *testing.T) {
	err := RunWorkspaceProviderUnsubscribe(config.WorkspaceProviderConfig{ID: "wf"}, SubscribeHookVars{ResourceID: "r", SessionName: "s"})
	if err == nil {
		t.Fatal("expected an error when the provider declares no unsubscribe hook")
	}
}
