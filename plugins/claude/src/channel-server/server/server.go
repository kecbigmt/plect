package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/kecbigmt/plecture/contracts/channel-protocol"
)

var permissionReplyRE = regexp.MustCompile(`(?i)^\s*(y|yes|n|no)\s+([a-km-z]{5})\s*$`)

// permissionTTL bounds how long an issued request_id stays answerable. A prompt
// the user never answers is pruned so the pending set cannot grow without bound
// and a long-stale id cannot be revived. Generous because a human may take a
// while to come back to a Slack thread.
const permissionTTL = 30 * time.Minute

// ChannelServer is an MCP channel server that bridges Claude Code and external message sources.
type ChannelServer struct {
	mcpServer *server.MCPServer
	sender    MessageSender

	// pending tracks the request_ids this server has actually issued (and not
	// yet consumed), keyed lowercase, with their issue time for TTL pruning. A
	// "y <id>" / "n <id>" message is honored as a verdict only when its id is in
	// here — a nonce the server itself minted — so a replayed or fabricated id
	// matching the regex is ignored. var-injected clock keeps the TTL testable.
	mu      sync.Mutex
	pending map[string]time.Time
	now     func() time.Time
}

// NewChannelServer creates the MCP server with channel capabilities and tools.
func NewChannelServer(sender MessageSender) *ChannelServer {
	s := &ChannelServer{
		sender:  sender,
		pending: make(map[string]time.Time),
		now:     time.Now,
	}

	s.mcpServer = server.NewMCPServer(
		"claude-channel-slack",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithInstructions(
			"Messages from Slack arrive as <channel source=\"claude-channel-slack\" user=\"...\" thread_ts=\"...\">. "+
				"When a Slack message asks you to do something, carry out the task; your turn's final "+
				"message is posted to the Slack thread automatically.",
		),
		server.WithExperimental(map[string]any{
			"claude/channel":            map[string]any{},
			"claude/channel/permission": map[string]any{},
		}),
	)

	// Register permission request notification handler
	s.mcpServer.AddNotificationHandler("notifications/claude/channel/permission_request", s.handlePermissionRequest)

	return s
}

// MCPServer returns the underlying MCP server for stdio transport.
func (s *ChannelServer) MCPServer() *server.MCPServer {
	return s.mcpServer
}

// OnMessage handles incoming messages from any source.
// It checks for permission reply patterns, otherwise forwards to Claude.
//
// A message is honored as a permission verdict only when all three hold:
//  1. its text is exactly "y|yes|n|no <id>" (the regex),
//  2. it carries a non-empty Source — i.e. it came from an adapter's
//     authenticated interactive-user path, not a system/content delivery, and
//  3. <id> is a request_id this server itself issued and has not yet consumed.
//
// Any one missing → the message is forwarded to Claude as ordinary text. This
// closes the forgery vector where anyone able to inject a "y <id>" message
// (e.g. publishing a content event) could approve a Claude permission request:
// content deliveries carry no Source, and a fabricated/replayed id is not a
// live nonce.
func (s *ChannelServer) OnMessage(msg protocol.MessagePayload) {
	if requestID, behavior, ok := s.permissionVerdict(msg); ok {
		s.mcpServer.SendNotificationToAllClients("notifications/claude/channel/permission", map[string]any{
			"request_id": requestID,
			"behavior":   behavior,
		})
		return
	}

	// Forward as channel notification
	s.mcpServer.SendNotificationToAllClients("notifications/claude/channel", map[string]any{
		"content": msg.Text,
		"meta": map[string]any{
			"user":      msg.User,
			"user_id":   msg.UserID,
			"thread_ts": msg.ThreadTS,
		},
	})
}

// permissionVerdict reports whether msg is a valid permission verdict and, if
// so, returns the canonical request_id and behavior ("allow"/"deny"). The three
// gates from OnMessage's doc are enforced here: regex shape, non-empty Source,
// and a live server-issued nonce (consumed on success so a verdict counts once).
func (s *ChannelServer) permissionVerdict(msg protocol.MessagePayload) (requestID, behavior string, ok bool) {
	m := permissionReplyRE.FindStringSubmatch(msg.Text)
	if m == nil || msg.Source == "" {
		return "", "", false
	}
	id, live := s.consumePending(m[2])
	if !live {
		return "", "", false
	}
	behavior = "allow"
	if strings.HasPrefix(strings.ToLower(m[1]), "n") {
		behavior = "deny"
	}
	return id, behavior, true
}

// rememberPending records a request_id the server just issued so a later reply
// can be validated against it. Pruning stale ids happens here (issue time is the
// only clock we touch) rather than on a timer.
func (s *ChannelServer) rememberPending(requestID string) {
	id := strings.ToLower(requestID)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, t := range s.pending {
		if now.Sub(t) > permissionTTL {
			delete(s.pending, k)
		}
	}
	s.pending[id] = now
}

// consumePending reports whether requestID is a live (issued, unexpired) nonce
// and, if so, removes it so a verdict is honored exactly once. Returns the
// canonical lowercase id to forward to Claude.
func (s *ChannelServer) consumePending(requestID string) (string, bool) {
	id := strings.ToLower(requestID)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.pending[id]
	if !ok || s.now().Sub(t) > permissionTTL {
		delete(s.pending, id)
		return "", false
	}
	delete(s.pending, id)
	return id, true
}

func (s *ChannelServer) handlePermissionRequest(ctx context.Context, notification mcp.JSONRPCNotification) {
	params := notification.Params.AdditionalFields
	requestID, _ := params["request_id"].(string)
	toolName, _ := params["tool_name"].(string)
	description, _ := params["description"].(string)

	// Mint the nonce: only a "y/n <request_id>" reply matching an id we issued
	// here will later be accepted as a verdict (see OnMessage).
	if requestID != "" {
		s.rememberPending(requestID)
	}

	text := fmt.Sprintf(
		"*Permission request*\nClaude wants to run `%s`: %s\n\nReply `y %s` to allow or `n %s` to deny.",
		toolName, description, requestID, requestID,
	)

	s.sender.SendPermissionPrompt(text)
}
