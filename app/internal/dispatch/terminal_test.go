package dispatch

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
)

func TestResolveTerminalOwner_FindsDeclaringNode(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	writeFile(t, filepath.Join(globalDir, "config.toml"), "schema_version = 2\n")
	writeFile(t, filepath.Join(globalDir, "tasks", "tmux.toml"), `
[tmux]
kind  = "effect"
scope = "run"

[tmux.setup]
type   = "shell"
script = "echo '{}'"

[tmux.health.alive]
type = "noop"

[tmux.terminal.send_text]
type    = "exec"
command = "tmux"
args    = ["send-keys", "-t", { from = "self.outputs.session_name" }, "--"]
`)
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
uses = "tmux"
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatal(err)
	}
	wf := workflows["coding"]
	s := &domain.Session{Name: "o/r-1"}

	owner := resolveTerminalOwner(slog.Default(), cfg, s, wf)
	if owner == nil || owner.NodeID != "tmux" {
		t.Fatalf("owner = %+v, want the tmux node", owner)
	}
	if _, err := owner.Ops.Verb("send_text"); err != nil {
		t.Fatalf("send_text: %v", err)
	}
}

func TestResolveTerminalOwner_NilWhenNoNodeDeclaresTerminal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	writeFile(t, filepath.Join(globalDir, "config.toml"), "schema_version = 2\n")
	writeFile(t, filepath.Join(globalDir, "tasks", "envfile.toml"), `
[envfile]
kind  = "effect"
scope = "session"

[envfile.setup]
type   = "shell"
script = "echo '{}'"
`)
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
uses = "envfile"
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatal(err)
	}
	wf := workflows["coding"]
	s := &domain.Session{Name: "o/r-1"}

	if owner := resolveTerminalOwner(slog.Default(), cfg, s, wf); owner != nil {
		t.Fatalf("owner = %+v, want none when no effect declares an interactive endpoint", owner)
	}
}

// TestResolveTerminalOwner_CompileFailureIsNonFatal guards the tolerant
// contract documented on resolveTerminalOwner: a broken plan (a workflow
// node the config no longer has a declaration for) must not stop the
// dispatcher from building — it only means the terminal capability is
// unavailable, logged rather than propagated.
func TestResolveTerminalOwner_CompileFailureIsNonFatal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".config", "plect")
	writeFile(t, filepath.Join(globalDir, "config.toml"), "schema_version = 2\n")
	writeFile(t, filepath.Join(globalDir, "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[[coding.nodes]]
uses = "missing"
`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	workflows, err := cfg.LoadWorkflows("")
	if err != nil {
		t.Fatal(err)
	}
	wf := workflows["coding"]
	s := &domain.Session{Name: "o/r-1"}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if owner := resolveTerminalOwner(logger, cfg, s, wf); owner != nil {
		t.Fatalf("owner = %+v, want none on compile failure", owner)
	}
	if !bytes.Contains(logs.Bytes(), []byte("compile plan failed")) {
		t.Errorf("expected a compile-failure warning to be logged, got %q", logs.String())
	}
}
