package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kecbigmt/plecture/app/internal/state"
)

// writeRevisionTask ships an effect task document whose setup output carries
// a revision, so a judge leaf recorded against an instance of it has an
// instance revision to compare against.
func writeRevisionTask(t *testing.T) {
	t.Helper()
	home := os.Getenv("HOME")
	doc := `
[revwork]
kind  = "effect"
scope = "session"

[revwork.setup]
type   = "shell"
script = "echo '{\"revision\":\"sha1\"}'"

[revwork.health.alive]
type = "noop"
`
	if err := os.WriteFile(filepath.Join(home, ".config", "plect", "tasks", "revwork.toml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setUpJudgeableInstance creates a session with one revwork instance carrying
// an "ac-met" judge leaf, ready for a judge tool call to record a verdict
// against.
func setUpJudgeableInstance(t *testing.T, sessionID string) (session, instance string) {
	t.Helper()
	writeRevisionTask(t)
	session = createPlainSession(t, sessionID)
	result, err := handleTaskSetup(context.Background(), reqWith(map[string]any{
		"task_id":        "revwork",
		"session":        session,
		"done_when_json": `{"all":[{"judge":"acceptance criteria","id":"ac-met"}]}`,
	}))
	if err != nil {
		t.Fatalf("handleTaskSetup: %v", err)
	}
	out := decodeJSONResult(t, result)
	instance, _ = out["instance"].(string)
	if instance == "" {
		t.Fatalf("missing instance in task_setup result: %+v", out)
	}
	return session, instance
}

func reqWith(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// The loop-engineering handlers must reject a missing required argument before
// touching config/state, so the error is a typed MCP error rather than a
// downstream panic on an empty session.
func TestLoopHandlers_RequireArgs(t *testing.T) {
	cases := []struct {
		name    string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
		args    map[string]any
	}{
		{"task_setup missing task_id", handleTaskSetup, map[string]any{}},
		{"task_cleanup missing instance", handleTaskCleanup, map[string]any{}},
		{"check missing session", handleCheck, map[string]any{}},
		{"tick missing session", handleTick, map[string]any{}},
		{"judge_approve missing session", handleJudgeApprove, map[string]any{"instance": "i", "judge_id": "j", "reason": "r"}},
		{"judge_approve missing instance", handleJudgeApprove, map[string]any{"session": "s", "judge_id": "j", "reason": "r"}},
		{"judge_approve missing judge_id", handleJudgeApprove, map[string]any{"session": "s", "instance": "i", "reason": "r"}},
		{"judge_request_changes missing session", handleJudgeRequestChanges, map[string]any{"instance": "i", "judge_id": "j", "reason": "r"}},
		{"subscribe missing resource", handleSubscribe, map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.handler(context.Background(), reqWith(tc.args))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("expected MCP error result, got %+v", result)
			}
		})
	}
}

// A caller still naming the retired reviewer_session argument must get an
// explicit rejection, not have it silently dropped and the verdict recorded
// under the ambient session instead. PLECT_SESSION_NAME is set to a real,
// valid session distinct from both the work session and the argument's
// (ignored) value, so a handler that fell back to the ambient session
// instead of rejecting would both succeed and misattribute the verdict —
// this test is red against that behavior, not just against a downstream
// session-lookup error.
func TestRecordJudge_RejectsRetiredReviewerSessionArgument(t *testing.T) {
	setUpConfigHome(t)
	work, instance := setUpJudgeableInstance(t, "judge-work-session")
	ambient := createPlainSession(t, "judge-ambient-session")
	t.Setenv("PLECT_SESSION_NAME", ambient)

	for _, tc := range []struct {
		name    string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
	}{
		{"judge_approve", handleJudgeApprove},
		{"judge_request_changes", handleJudgeRequestChanges},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.handler(context.Background(), reqWith(map[string]any{
				"session": work, "instance": instance, "judge_id": "ac-met", "reason": "r",
				"reviewer_session": "some-other-session",
			}))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("expected MCP error result for retired reviewer_session argument, got %+v", result)
			}
			if !strings.Contains(extractErrorText(result), "judge_session") {
				t.Fatalf("error text = %q, want it to name judge_session", extractErrorText(result))
			}

			s := state.NewStore("").Get(work)
			if s == nil {
				t.Fatal("work session missing")
			}
			if st := s.Tasks[instance]; st != nil && st.DoneWhen != nil && st.DoneWhen.Judges["ac-met"] != nil {
				t.Fatalf("verdict must not be persisted when the retired argument is rejected, got %+v", st.DoneWhen.Judges["ac-met"])
			}
		})
	}
}

// TestHandleJudgeApprove_JudgeSessionRoundTrips proves the judge_session
// argument recorded through the MCP tool round-trips end to end: the
// response carries it back under judge_session (never reviewer_session),
// and the persisted contract state names the judge the same way.
func TestHandleJudgeApprove_JudgeSessionRoundTrips(t *testing.T) {
	setUpConfigHome(t)
	work, instance := setUpJudgeableInstance(t, "judge-work-session-2")
	reviewer := createPlainSession(t, "judge-review-session")

	result, err := handleJudgeApprove(context.Background(), reqWith(map[string]any{
		"session":       work,
		"instance":      instance,
		"judge_id":      "ac-met",
		"reason":        "looks good",
		"judge_session": reviewer,
	}))
	if err != nil {
		t.Fatalf("handleJudgeApprove: %v", err)
	}
	out := decodeJSONResult(t, result)
	if out["judge_session"] != reviewer {
		t.Fatalf("response judge_session = %v, want %v", out["judge_session"], reviewer)
	}
	if _, ok := out["reviewer_session"]; ok {
		t.Fatalf("response must not carry a reviewer_session key: %+v", out)
	}

	s := state.NewStore("").Get(work)
	if s == nil {
		t.Fatal("work session not persisted")
	}
	judge := s.Tasks[instance].DoneWhen.Judges["ac-met"]
	if judge == nil {
		t.Fatal("judge verdict not persisted")
	}
	if judge.JudgeSession != reviewer {
		t.Fatalf("persisted judge_session = %q, want %q", judge.JudgeSession, reviewer)
	}
}

func TestGetStringMapArg(t *testing.T) {
	req := reqWith(map[string]any{
		"inputs": map[string]any{
			"intent": "work",
			"count":  float64(3), // JSON numbers decode as float64
		},
	})
	got := getStringMapArg(req, "inputs")
	if got["intent"] != "work" {
		t.Errorf("intent = %q, want work", got["intent"])
	}
	if got["count"] != "3" {
		t.Errorf("count = %q, want 3", got["count"])
	}

	if getStringMapArg(reqWith(map[string]any{}), "inputs") != nil {
		t.Error("expected nil for absent inputs")
	}
}

// Each loop-engineering tool must carry the canonical name the dispatch table
// and instrumentation key on.
func TestLoopTools_Names(t *testing.T) {
	want := map[*mcp.Tool]string{
		&taskSetupTool:           "plect_task_setup",
		&taskCleanupTool:         "plect_task_cleanup",
		&checkTool:               "plect_check",
		&tickTool:                "plect_tick",
		&judgeApproveTool:        "plect_judge_approve",
		&judgeRequestChangesTool: "plect_judge_request_changes",
		&subscribeTool:           "plect_subscribe",
	}
	for tool, name := range want {
		if tool.Name != name {
			t.Errorf("tool name = %q, want %q", tool.Name, name)
		}
	}
}
