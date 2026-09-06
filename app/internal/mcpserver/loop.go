package mcpserver

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
)

// Loop-engineering surface: the typed MCP counterparts of the CLI task / gate /
// subscribe operations. Without these an agent would shell out to the CLI to
// instantiate dynamic tasks or record judge results, leaving MCP an incomplete
// interface. Sessions default to the ambient pane env in the service
// layer, so an agent may omit `session` when acting on its own session.

var taskSetupTool = mcp.NewTool("plect_task_setup",
	mcp.WithDescription("Instantiate a task definition against a live session at runtime (ADR-003 dynamic instantiation): its setup runs, the instance (outputs + cleanup + scope) is registered in session state and shown by plect_status, and teardown reclaims it. run-scoped tasks may only be instantiated while the run scope is up; session-scoped tasks any time. Without `name` each setup creates a fresh numbered instance (<task>#<n>); with `name` it pins a session-global singleton that collides on re-setup."),
	mcp.WithString("task_id",
		mcp.Required(),
		mcp.Description("Task definition id to instantiate (matches the task .toml stem)"),
	),
	mcp.WithString("session",
		mcp.Description("Target session name (defaults to PLECT_SESSION_NAME)"),
	),
	mcp.WithString("name",
		mcp.Description("Instance identity: the key becomes the name (session-global singleton); a second setup of the same name is a collision error. Omit for a fresh numbered <task>#<n>."),
	),
	mcp.WithString("resource",
		mcp.Description("External resource bound to the instance (exposed to its setup/done_when as .ResourceID); not part of the instance key"),
	),
	mcp.WithObject("inputs",
		mcp.Description("Input bindings as a JSON object (key=value). Bound ahead of the workflow/workspace-provider outputs and session inputs."),
	),
	mcp.WithString("done_when_json",
		mcp.Description("Additional done_when JSON appended to this instance only (e.g. an extra judge leaf)"),
	),
)

var taskCleanupTool = mcp.NewTool("plect_task_cleanup",
	mcp.WithDescription("Reclaim one dynamic task instance: run its cleanup script and remove it from session state. The single-instance counterpart of plect_down / plect_destroy. Addressed by its key alone (a name or a numbered <task>#<n>) and reclaimed regardless of which task produced it. A missing instance is a no-op success, so cleanup-then-setup is a safe recreate idiom."),
	mcp.WithString("instance",
		mcp.Required(),
		mcp.Description("Instance key to reclaim (a name, e.g. \"initial\", or a numbered key, e.g. \"review#1\")"),
	),
	mcp.WithString("session",
		mcp.Description("Target session name (defaults to PLECT_SESSION_NAME)"),
	),
)

var taskFinalizeTool = mcp.NewTool("plect_task_finalize",
	mcp.WithDescription("Finalize a task instance (ADR \"goal-as-task\" D4): reconfirm its done_when is satisfied at the current revision (refusing, with no record made, if it is not), then let the bound --resource's definition record completion via its finalize script if it declares one. Gate + record only — the instance is left in place; run plect_task_cleanup separately afterward to reclaim it. A resource definition without a finalize script is a no-op step, not an error."),
	mcp.WithString("instance",
		mcp.Required(),
		mcp.Description("Instance key to finalize (a name, e.g. \"initial\", or a numbered key, e.g. \"review#1\")"),
	),
	mcp.WithString("session",
		mcp.Description("Target session name (defaults to PLECT_SESSION_NAME)"),
	),
)

var checkTool = mcp.NewTool("plect_check",
	mcp.WithDescription("Observation-only: evaluate each done_when-bearing task instance for a session — and, against those same facts, its [[chains]] — and report the result, with zero side effects — no heartbeat budget advance, no event published, no session woken or spawned, and no observation and no dynamic output refresh (reads whatever plect_tick, or the initial produce, last persisted). Repeated calls never change session state. Returns one action per instance: satisfied, wait, review_required, kick, or escalate — each with heartbeat_budget, heartbeat_ticks, a fingerprint for unchanged-poll detection, and unmet_items carrying machine-readable check/judge state. Also returns one chains[] entry per (chain, instance): fired/already-active/blocked (with blocked_reason), never spawned. Use plect_tick to actually observe, advance the gate, and fire chains."),
	mcp.WithString("session",
		mcp.Required(),
		mcp.Description("Resource identifier, or session name (e.g. session-123)"),
	),
)

var tickTool = mcp.NewTool("plect_tick",
	mcp.WithDescription("The Goal Loop actuator: evaluate each done_when-bearing task instance for a session and act on the result — heartbeat-triggered ticks consume its heartbeat budget, while event and manual ticks do not. It publishes the resulting kickback/review/escalation event, and pushes a done/escalate terminal event to the parent exactly once per instance. Against that same fact set, also fires [[chains]]: a chain whose when holds and whose wired outputs are present spawns its workflow (idempotent — an already-active target is reported, not re-spawned). Returns one action per instance: satisfied, wait, review_required, kick, or escalate — each with heartbeat_budget, heartbeat_ticks, a fingerprint for unchanged-poll detection, and unmet_items carrying machine-readable check/judge state. Also returns one chains[] entry per (chain, instance) with its fired/spawned/already-active/blocked outcome."),
	mcp.WithString("session",
		mcp.Required(),
		mcp.Description("Resource identifier, or session name (e.g. session-123)"),
	),
	mcp.WithBoolean("no_refresh",
		mcp.Description("Decide against the last observation, without observing each resource first"),
	),
)

var judgeApproveTool = mcp.NewTool("plect_judge_approve",
	mcp.WithDescription("Record an approve action for one done_when judge leaf (verification gate). Records against the instance revision so a later revision reopens the gate. Provenance-constrained judges require judge_session to match the ambient judge pane."),
	mcp.WithString("session",
		mcp.Required(),
		mcp.Description("Resource identifier, or session name being reviewed"),
	),
	mcp.WithString("instance",
		mcp.Required(),
		mcp.Description("Task instance key carrying the judge leaf"),
	),
	mcp.WithString("judge_id",
		mcp.Required(),
		mcp.Description("done_when judge leaf id to record against"),
	),
	mcp.WithString("reason",
		mcp.Required(),
		mcp.Description("Reason for this judge action"),
	),
	mcp.WithString("revision",
		mcp.Description("Opaque revision reviewed (defaults to the instance revision output)"),
	),
	mcp.WithString("judge_session",
		mcp.Description("Judge session name (defaults to PLECT_SESSION_NAME; provenance-constrained judges require it to match the ambient judge pane)"),
	),
)

var judgeRequestChangesTool = mcp.NewTool("plect_judge_request_changes",
	mcp.WithDescription("Record a request-changes action for one done_when judge leaf (verification gate). Holds the gate unsatisfied until a new revision is approved. Provenance-constrained judges require judge_session to match the ambient judge pane."),
	mcp.WithString("session",
		mcp.Required(),
		mcp.Description("Resource identifier, or session name being reviewed"),
	),
	mcp.WithString("instance",
		mcp.Required(),
		mcp.Description("Task instance key carrying the judge leaf"),
	),
	mcp.WithString("judge_id",
		mcp.Required(),
		mcp.Description("done_when judge leaf id to record against"),
	),
	mcp.WithString("reason",
		mcp.Required(),
		mcp.Description("Reason for this judge action"),
	),
	mcp.WithString("revision",
		mcp.Description("Opaque revision reviewed (defaults to the instance revision output)"),
	),
	mcp.WithString("judge_session",
		mcp.Description("Judge session name (defaults to PLECT_SESSION_NAME; provenance-constrained judges require it to match the ambient judge pane)"),
	),
)

var subscribeTool = mcp.NewTool("plect_subscribe",
	mcp.WithDescription("Subscribe a live session to an opaque resource so its events (for a code review resource: CI status, review decisions, state changes) arrive in this session's event stream, readable with plect_event_list. Additive — never replaces existing subscriptions, and subscribing a resource another session already watches does not take it over."),
	mcp.WithString("resource",
		mcp.Required(),
		mcp.Description("Resource id to subscribe to"),
	),
	mcp.WithString("session",
		mcp.Description("Subscriber session name (defaults to PLECT_SESSION_NAME)"),
	),
)

func handleTaskSetup(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	taskID := request.GetString("task_id", "")
	if taskID == "" {
		return mcp.NewToolResultError("task_id is required"), nil
	}

	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	result, err := service.TaskSetup(cfg, state.NewStore(""), service.TaskSetupParams{
		TaskID:            taskID,
		SessionName:       request.GetString("session", ""),
		Name:              request.GetString("name", ""),
		Resource:          request.GetString("resource", ""),
		Inputs:            getStringMapArg(request, "inputs"),
		ExtraDoneWhenJSON: request.GetString("done_when_json", ""),
	})
	if err != nil {
		return errorResult(err), nil
	}

	return jsonResult(map[string]any{
		"ok":              true,
		"session_name":    result.SessionName,
		"instance":        result.Instance,
		"task_id":         result.TaskID,
		"scope":           result.Scope,
		"name":            result.Name,
		"resource":        result.Resource,
		"subscribed":      result.Subscribed,
		"subscribe_error": result.SubscribeError,
		"outputs":         result.Outputs,
	})
}

func handleTaskCleanup(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	instance := request.GetString("instance", "")
	if instance == "" {
		return mcp.NewToolResultError("instance is required"), nil
	}

	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	result, err := service.TaskCleanup(cfg, state.NewStore(""), service.TaskCleanupParams{
		Instance:    instance,
		SessionName: request.GetString("session", ""),
	})
	if err != nil {
		return errorResult(err), nil
	}

	return jsonResult(map[string]any{
		"ok":                true,
		"session_name":      result.SessionName,
		"instance":          result.Instance,
		"found":             result.Found,
		"unsubscribed":      result.Unsubscribed,
		"unsubscribe_error": result.UnsubscribeError,
	})
}

func handleTaskFinalize(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	instance := request.GetString("instance", "")
	if instance == "" {
		return mcp.NewToolResultError("instance is required"), nil
	}

	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	result, err := service.FinalizeTask(cfg, state.NewStore(""), service.FinalizeTaskParams{
		Instance:    instance,
		SessionName: request.GetString("session", ""),
	})
	if err != nil {
		return errorResult(err), nil
	}

	return jsonResult(map[string]any{
		"ok":                  true,
		"session_name":        result.SessionName,
		"instance":            result.Instance,
		"resource_id":         result.ResourceID,
		"resource_definition": result.Definition,
		"finalized":           result.Finalized,
		"next_step":           fmt.Sprintf("run plect_task_cleanup on %q to reclaim it", result.Instance),
	})
}

func handleCheck(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	session := request.GetString("session", "")
	if session == "" {
		return mcp.NewToolResultError("session is required"), nil
	}

	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	result, err := service.CheckSession(cfg, state.NewStore(""), service.CheckParams{
		SessionName: session,
	})
	if err != nil {
		return errorResult(err), nil
	}

	return jsonResult(map[string]any{
		"ok":      true,
		"actions": result.Actions,
		"chains":  result.Chains,
	})
}

func handleTick(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	session := request.GetString("session", "")
	if session == "" {
		return mcp.NewToolResultError("session is required"), nil
	}

	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	result, err := service.TickSession(cfg, state.NewStore(""), service.TickParams{
		SessionName: session,
		SkipRefresh: request.GetBool("no_refresh", false),
	})
	if err != nil {
		return errorResult(err), nil
	}

	return jsonResult(map[string]any{
		"ok":      true,
		"actions": result.Actions,
		"chains":  result.Chains,
	})
}

func handleJudgeApprove(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return recordJudge(request, task.JudgeActionApprove)
}

func handleJudgeRequestChanges(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return recordJudge(request, task.JudgeActionRequestChanges)
}

func recordJudge(request mcp.CallToolRequest, action string) (*mcp.CallToolResult, error) {
	session := request.GetString("session", "")
	if session == "" {
		return mcp.NewToolResultError("session is required"), nil
	}
	instance := request.GetString("instance", "")
	if instance == "" {
		return mcp.NewToolResultError("instance is required"), nil
	}
	judgeID := request.GetString("judge_id", "")
	if judgeID == "" {
		return mcp.NewToolResultError("judge_id is required"), nil
	}

	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	result, err := service.RecordJudge(cfg, state.NewStore(""), service.JudgeParams{
		SessionName:  session,
		Instance:     instance,
		LeafID:       judgeID,
		Action:       action,
		Reason:       request.GetString("reason", ""),
		Revision:     request.GetString("revision", ""),
		JudgeSession: request.GetString("judge_session", ""),
	})
	if err != nil {
		return errorResult(err), nil
	}

	return jsonResult(map[string]any{
		"ok":            true,
		"session_name":  result.SessionName,
		"instance":      result.Instance,
		"leaf_id":       result.LeafID,
		"action":        result.Action,
		"revision":      result.Revision,
		"judge_session": result.JudgeSession,
	})
}

func handleSubscribe(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resource := request.GetString("resource", "")
	if resource == "" {
		return mcp.NewToolResultError("resource is required"), nil
	}

	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	if err := service.Subscribe(cfg, state.NewStore(""), service.SubscribeParams{
		ResourceID:  resource,
		SessionName: request.GetString("session", ""),
	}); err != nil {
		return errorResult(err), nil
	}

	return jsonResult(map[string]any{
		"ok":       true,
		"resource": resource,
	})
}

// getStringMapArg reads the named object argument as map[string]string. Task
// inputs are key=value bindings, so non-string values are stringified rather
// than dropped.
func getStringMapArg(request mcp.CallToolRequest, name string) map[string]string {
	obj := getObjectArg(request, name)
	if obj == nil {
		return nil
	}
	out := make(map[string]string, len(obj))
	for k, v := range obj {
		if s, ok := v.(string); ok {
			out[k] = s
			continue
		}
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
