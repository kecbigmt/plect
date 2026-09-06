package lang

import "testing"

func TestParseActionExec(t *testing.T) {
	a, err := ParseAction(map[string]any{
		"type":  "exec",
		"bin":   "okf-goal",
		"args":  []any{"task", "bootstrap", map[string]any{"from": "inputs.owner"}},
		"stdin": map[string]any{"json": map[string]any{"from": "judges"}},
	}, Position{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != "exec" || a.Bin != "okf-goal" || a.Command != "" {
		t.Fatalf("got %+v", a)
	}
	if len(a.Args) != 3 || a.Args[0].Literal != "task" || a.Args[2].From != "inputs.owner" {
		t.Fatalf("args: got %+v", a.Args)
	}
	if a.Stdin == nil || a.Stdin.Form != FormJSON {
		t.Fatalf("stdin: got %+v", a.Stdin)
	}
}

func TestParseActionShell(t *testing.T) {
	a, err := ParseAction(map[string]any{
		"type":   "shell",
		"script": "\"$activity_bin\" reset \"$session_name\"\n",
		"bind": map[string]any{
			"session_name": map[string]any{"from": "session.name"},
			"send_text":    map[string]any{"terminal": "send_text"},
			"activity_bin": map[string]any{"bin": "codex-agent-activity"},
		},
	}, Position{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != "shell" || a.Script == "" {
		t.Fatalf("got %+v", a)
	}
	if a.Bind["send_text"].Terminal != "send_text" || a.Bind["activity_bin"].Bin != "codex-agent-activity" {
		t.Fatalf("bind: got %+v", a.Bind)
	}
}

func TestParseActionRejections(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		code Code
	}{
		{"an unknown type", map[string]any{"type": "python", "script": "print('hi')"}, CodeActionTypeUnknown},
		{"noop, legal only through ParseAliveAction", map[string]any{"type": "noop"}, CodeActionTypeUnknown},
		{"no type", map[string]any{"bin": "okf-goal"}, CodeFieldRequired},
		{"a field from the other variant", map[string]any{"type": "shell", "script": "true", "args": []any{"--flag"}}, CodeActionVariant},
		{"a shell field on an exec action", map[string]any{"type": "exec", "bin": "x", "bind": map[string]any{}}, CodeActionVariant},
		{"both bin and command", map[string]any{"type": "exec", "command": "curl", "bin": "slack-post"}, CodeActionBinAndCommand},
		{"neither bin nor command", map[string]any{"type": "exec", "args": []any{"x"}}, CodeActionBinAndCommand},
		{"a computed command", map[string]any{"type": "exec", "command": map[string]any{"from": "inputs.command"}}, CodeRefDynamic},
		{"interpolation in shell source", map[string]any{"type": "shell", "script": "echo {{.SessionName}}\n"}, CodeShellInterpolation},
		{"no script on a shell action", map[string]any{"type": "shell"}, CodeFieldRequired},
		{"an unknown field", map[string]any{"type": "exec", "bin": "x", "cwd": "/tmp"}, CodeFieldUnknown},
		{"a capability tag is fine in argv, but not a bare table", map[string]any{"type": "exec", "bin": "x", "args": []any{map[string]any{"nope": 1}}}, CodeValueTagUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAction(tc.raw, Position{})
			wantDiag(t, err, tc.code, LayerStructural)
		})
	}
}

func TestParseAliveActionNoop(t *testing.T) {
	a, err := ParseAliveAction(map[string]any{"type": "noop"}, Position{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != ActionNoop {
		t.Fatalf("got %+v", a)
	}
	if got := a.Source(); got != "noop" {
		t.Errorf("Source() = %q, want %q", got, "noop")
	}
}

func TestParseAliveActionRejectsFieldsOnNoop(t *testing.T) {
	_, err := ParseAliveAction(map[string]any{"type": "noop", "script": "true"}, Position{})
	wantDiag(t, err, CodeFieldUnknown, LayerStructural)
}

// ParseAliveAction delegates everything but noop to ParseAction, so exec and
// shell behave identically through either entry point.
func TestParseAliveActionDelegatesOtherVariants(t *testing.T) {
	a, err := ParseAliveAction(map[string]any{"type": "exec", "bin": "gh-guard"}, Position{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != ActionExec || a.Bin != "gh-guard" {
		t.Fatalf("got %+v", a)
	}
}

func TestParseActionAcceptsACapabilityInArgv(t *testing.T) {
	a, err := ParseAction(map[string]any{
		"type": "exec",
		"bin":  "relay",
		"args": []any{map[string]any{"terminal": "send_text"}},
	}, Position{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Args[0].Form != FormTerminal {
		t.Errorf("an argv element of an action that accepts one may consume a capability: got %+v", a.Args[0])
	}
}
