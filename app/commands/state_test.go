package commands

import (
	"strings"
	"testing"
)

// The facts a chat thread's setup once copied into Conversation already live
// under the effect node's own outputs (thread_ts, channel_id, permalink), so
// there is nothing left for this command to write.
func TestStateCommandHasNoSetConversationSubcommand(t *testing.T) {
	for _, cmd := range stateCmd.Commands() {
		if cmd.Name() == "set-conversation" {
			t.Fatal("state command must not expose set-conversation")
		}
	}
}

func TestSetOutputHelpDocumentsRuntimeTaskTarget(t *testing.T) {
	if !strings.Contains(setOutputCmd.Long, "--task <task-handle>") {
		t.Fatalf("set-output long help must document --task <task-handle>; got:\n%s", setOutputCmd.Long)
	}
	flag := setOutputCmd.Flags().Lookup("task")
	if flag == nil {
		t.Fatal("set-output missing --task flag")
	}
	if flag.Usage != "Target a produced runtime task such as review#1" {
		t.Fatalf("--task usage = %q", flag.Usage)
	}
	if !strings.Contains(setOutputCmd.Long, "plect state set-output session-1 --task review#1") {
		t.Fatalf("set-output examples must show --task review#1; got:\n%s", setOutputCmd.Long)
	}
}

// A payload that is not a JSON object is refused before anything is loaded or
// written: the command's argument is an object of state keys, and "null" or a
// list is a caller mistake, not an empty write.
func TestSetStateRejectsAPayloadThatIsNotAnObject(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "malformed", payload: `{"verdict_revision":`},
		{name: "null", payload: `null`},
		{name: "a list", payload: `["verdict_revision"]`},
		{name: "a bare string", payload: `"sha2"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := setStateCmd.RunE(setStateCmd, []string{"session-1", tt.payload})
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(err.Error(), "not a JSON object") {
				t.Errorf("error = %v, want it to say the payload is not a JSON object", err)
			}
		})
	}
}

func TestSetStateHelpDocumentsTheInstanceTarget(t *testing.T) {
	flag := setStateCmd.Flags().Lookup("instance")
	if flag == nil {
		t.Fatal("state set missing --instance flag")
	}
	if !strings.Contains(setStateCmd.Long, "self.state.<key>") {
		t.Errorf("state set long help must say which root reads what it writes; got:\n%s", setStateCmd.Long)
	}
	if !strings.Contains(setStateCmd.Long, "--instance review#1") {
		t.Errorf("state set examples must show the instance target; got:\n%s", setStateCmd.Long)
	}
}
