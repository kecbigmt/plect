package config

import (
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

func terminalVerbAction(script string) *lang.Action {
	return &lang.Action{Type: lang.ActionShell, Script: script}
}

// A verb is available on its own: an effect may offer a capture and nothing
// else, and what a consumer asks for either resolves or fails where it is
// consumed.
func TestTerminalConfigVerb(t *testing.T) {
	full := &TerminalConfig{
		Attach:   terminalVerbAction("a"),
		Capture:  terminalVerbAction("c"),
		SendText: terminalVerbAction("t"),
		SendKeys: terminalVerbAction("k"),
		PID:      terminalVerbAction("p"),
	}
	tests := []struct {
		name    string
		term    *TerminalConfig
		verb    string
		wantErr string
	}{
		{name: "declared verb resolves", term: full, verb: "capture"},
		{name: "pid resolves", term: full, verb: "pid"},
		{name: "no endpoint at all", term: nil, verb: "capture", wantErr: "capture"},
		{name: "bare table declares no verb", term: &TerminalConfig{}, verb: "attach", wantErr: "attach"},
		{
			name:    "an undeclared verb of a partial table",
			term:    &TerminalConfig{Capture: terminalVerbAction("c")},
			verb:    "send_text",
			wantErr: "send_text",
		},
		{name: "an unknown verb name", term: full, verb: "sendtext", wantErr: "want attach"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, err := tt.term.Verb(tt.verb)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Verb(%q) = %v", tt.verb, err)
				}
				if action == nil {
					t.Fatalf("Verb(%q) resolved to no action", tt.verb)
				}
				return
			}
			if err == nil {
				t.Fatalf("Verb(%q) resolved, want an error", tt.verb)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// A partial table now loads: availability is per verb, so an effect offering
// only the verbs it can honor is a legitimate declaration.
func TestLoadTaskDefinitions_TerminalVerbsAreIndependent(t *testing.T) {
	cfg := writeEffectDoc(t, t.TempDir(), "pane", `
[pane]
kind  = "effect"
scope = "run"

[pane.setup]
type   = "shell"
script = "echo '{}'"

[pane.health.alive]
type = "noop"

[pane.terminal.attach]
type    = "exec"
command = "tmux"
args    = ["attach", "-t", { from = "self.outputs.session_name" }]

[pane.terminal.capture]
type    = "exec"
command = "tmux"
args    = ["capture-pane", "-p", "-t", { from = "self.outputs.session_name" }]

[pane.terminal.pid]
type    = "exec"
command = "tmux"
args    = ["display-message", "-p", "-t", { from = "self.outputs.session_name" }, "#{pane_pid}"]
`)
	defs, err := cfg.LoadTaskDefinitions("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	def, ok := defs["pane"]
	if !ok {
		t.Fatal("pane effect not loaded")
	}
	if !def.Terminal.IsDeclared() {
		t.Fatal("expected the terminal table to read as declared")
	}
	if _, err := def.Terminal.Verb("capture"); err != nil {
		t.Errorf("capture: %v", err)
	}
	if _, err := def.Terminal.Verb("pid"); err != nil {
		t.Errorf("pid: %v", err)
	}
	if _, err := def.Terminal.Verb("send_text"); err == nil {
		t.Error("send_text resolved, but this effect declares no such verb")
	}
}

// A typo'd verb name must fail to load rather than vanish as an unread key,
// which would leave a consumer of the real verb looking undeclared with no
// hint why.
func TestLoadTaskDefinitions_TerminalUnknownFieldRejected(t *testing.T) {
	cfg := writeEffectDoc(t, t.TempDir(), "pane", `
[pane]
kind  = "effect"
scope = "run"

[pane.setup]
type   = "shell"
script = "echo '{}'"

[pane.health.alive]
type = "noop"

[pane.terminal.sendtext]
type   = "shell"
script = "true"
`)
	_, err := cfg.LoadTaskDefinitions("")
	if err == nil {
		t.Fatal("expected an error for an unknown terminal verb")
	}
	if !strings.Contains(err.Error(), "sendtext") {
		t.Errorf("error %q should name the unknown verb", err.Error())
	}
}
