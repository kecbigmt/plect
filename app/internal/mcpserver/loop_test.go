package mcpserver

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

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
// under the ambient session instead.
func TestRecordJudge_RejectsRetiredReviewerSessionArgument(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
	}{
		{"judge_approve", handleJudgeApprove},
		{"judge_request_changes", handleJudgeRequestChanges},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.handler(context.Background(), reqWith(map[string]any{
				"session": "s", "instance": "i", "judge_id": "j", "reason": "r",
				"reviewer_session": "reviewer-1",
			}))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil || !result.IsError {
				t.Fatalf("expected MCP error result for retired reviewer_session argument, got %+v", result)
			}
		})
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
