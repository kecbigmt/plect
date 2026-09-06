package mcpserver

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

var eventListTool = mcp.NewTool("plect_event_list",
	mcp.WithDescription("List events recorded for a session's durable timeline (resource changes, lifecycle, messages, replies, notes). Works for destroyed sessions too — history is retained. Pass subtree to see a session tree (root + descendants) merged in time order — the canonical cross-session scope. session and subtree are mutually exclusive."),
	mcp.WithString("session",
		mcp.Description("Resource identifier, or session name (e.g. session-123). Omit when using subtree."),
	),
	mcp.WithString("subtree",
		mcp.Description("Root session (URL or name): list events for the session tree rooted there (root + descendants), in time order. Mutually exclusive with session."),
	),
	mcp.WithString("types", mcp.Description("Comma-separated type globs to include (e.g. \"widget.*,user.emit\")")),
	mcp.WithString("source", mcp.Description("Comma-separated sources to include (e.g. \"cli,mcp\")")),
	mcp.WithString("direction", mcp.Description("Filter by direction: inbound|outbound|internal")),
	mcp.WithString("order", mcp.Description("List order: asc (oldest first, paginates via next_cursor) or desc (newest first, most recent page). Default asc.")),
	mcp.WithString("cursor", mcp.Description("Opaque pagination token from a prior page's next_cursor (asc only); empty = first page")),
	mcp.WithNumber("limit", mcp.Description("Max events to return; 0 = all")),
	mcp.WithToolAnnotation(mcp.ToolAnnotation{ReadOnlyHint: boolPtr(true)}),
)

var eventShowTool = mcp.NewTool("plect_event_show",
	mcp.WithDescription("Show a single event by id from a session's durable timeline. Works for destroyed sessions too — history is retained."),
	mcp.WithString("session",
		mcp.Required(),
		mcp.Description("Resource identifier, or session name (e.g. session-123)"),
	),
	mcp.WithString("event_id",
		mcp.Required(),
		mcp.Description("Event id, as returned by plect_event_list"),
	),
	mcp.WithToolAnnotation(mcp.ToolAnnotation{ReadOnlyHint: boolPtr(true)}),
)

var eventPublishTool = mcp.NewTool("plect_event_publish",
	mcp.WithDescription("Publish an event to a session's timeline (durably recorded). Workflow channels deliver subscribed event types such as user.emit to the session runtime and any connected chat thread."),
	mcp.WithString("session",
		mcp.Required(),
		mcp.Description("Resource identifier, or session name (e.g. session-123)"),
	),
	mcp.WithString("type", mcp.Required(), mcp.Description("Event type (free-form dotted topic, e.g. user.emit)")),
	mcp.WithString("source", mcp.Description("Event source (default: mcp)")),
	mcp.WithString("direction", mcp.Description("inbound|outbound|internal")),
	mcp.WithString("summary", mcp.Description("One-line summary for the timeline")),
	mcp.WithString("body", mcp.Description("Full body text")),
	mcp.WithObject("metadata", mcp.Description("Arbitrary string key/value metadata")),
)

func handleEventList(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	store := state.NewStore("")
	session := request.GetString("session", "")
	subtree := request.GetString("subtree", "")
	scopes := 0
	for _, s := range []string{session, subtree} {
		if s != "" {
			scopes++
		}
	}
	if scopes == 0 {
		return mcp.NewToolResultError("session or subtree is required"), nil
	}
	if scopes > 1 {
		return mcp.NewToolResultError("session and subtree are mutually exclusive; each names a different scope"), nil
	}
	order, err := event.NormalizeOrder(request.GetString("order", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	f := event.Filter{
		Types:     event.SplitCSV(request.GetString("types", "")),
		Sources:   event.SplitCSV(request.GetString("source", "")),
		Direction: event.Direction(request.GetString("direction", "")),
		Limit:     request.GetInt("limit", 0),
	}
	params := service.EventPageParams{
		Order:  order,
		Cursor: request.GetString("cursor", ""),
		Filter: f,
	}
	var page service.EventPageResult
	switch {
	case subtree != "":
		page, err = service.EventPageSubtree(cfg, store, subtree, params)
	default:
		page, err = service.EventPage(cfg, store, session, params)
	}
	if err != nil {
		return errorResult(err), nil
	}
	out := map[string]any{"ok": true, "events": page.Events}
	if page.NextCursor != "" {
		out["next_cursor"] = page.NextCursor
	}
	return jsonResult(out)
}

func handleEventShow(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	store := state.NewStore("")
	session := request.GetString("session", "")
	if session == "" {
		return mcp.NewToolResultError("session is required"), nil
	}
	eventID := request.GetString("event_id", "")
	if eventID == "" {
		return mcp.NewToolResultError("event_id is required"), nil
	}
	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	ev, err := service.EventShow(cfg, store, session, eventID)
	if err != nil {
		return errorResult(err), nil
	}
	return jsonResult(map[string]any{"ok": true, "event": ev})
}

func handleEventPublish(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// delivery_mode named a retired input: silently dropping it would tell a
	// caller their delivery preference is honored when it no longer does
	// anything — delivery is derived from the published type's own prefix.
	if _, ok := request.GetArguments()["delivery_mode"]; ok {
		return mcp.NewToolResultError(fmt.Sprintf("delivery_mode is no longer accepted; delivery is derived from the event type's %q prefix", event.TypeTerminalPrefix)), nil
	}
	store := state.NewStore("")
	session := request.GetString("session", "")
	if session == "" {
		return mcp.NewToolResultError("session is required"), nil
	}
	source := request.GetString("source", "")
	if source == "" {
		source = event.SourceMCP
	}
	cfg, err := config.Load()
	if err != nil {
		return errorResult(err), nil
	}
	ev, err := service.EventPublish(cfg, store, session, service.EventPublishParams{
		Type:      request.GetString("type", ""),
		Source:    source,
		Direction: event.Direction(request.GetString("direction", "")),
		Summary:   request.GetString("summary", ""),
		Body:      request.GetString("body", ""),
		Metadata:  stringMap(getObjectArg(request, "metadata")),
	})
	if err != nil {
		return errorResult(err), nil
	}
	return jsonResult(map[string]any{"ok": true, "event": ev})
}

// stringMap coerces an MCP object argument (map[string]any) to map[string]string.
func stringMap(m map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}
