package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kecbigmt/plecture/contracts/channel-protocol"
)

// newPermissionRequestNotification builds the MCP notification channel-server
// receives from Claude for a permission request carrying request_id.
func newPermissionRequestNotification(requestID string) mcp.JSONRPCNotification {
	var n mcp.JSONRPCNotification
	n.Params.AdditionalFields = map[string]any{
		"request_id":  requestID,
		"tool_name":   "Bash",
		"description": "run a command",
	}
	return n
}

type mockSender struct {
	mu    sync.Mutex
	perms []string
}

func (m *mockSender) SendPermissionPrompt(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.perms = append(m.perms, text)
	return nil
}

func TestChannelServer_OnMessage_ForwardsToMCP(t *testing.T) {
	sender := &mockSender{}
	cs := NewChannelServer(sender)

	// Verify the MCP server was created
	if cs.MCPServer() == nil {
		t.Fatal("MCPServer() should not be nil")
	}

	// OnMessage should not panic for a normal message
	cs.OnMessage(protocol.MessagePayload{
		User:     "testuser",
		UserID:   "U123",
		Text:     "hello",
		ThreadTS: "1234567890.123456",
	})
}

func TestChannelServer_PermissionVerdict(t *testing.T) {
	const id = "abcde"
	slackReply := func(text string) protocol.MessagePayload {
		return protocol.MessagePayload{UserID: "U123", Text: text, ThreadTS: "1.1", Source: "slack"}
	}

	t.Run("authenticated reply to an issued nonce is honored", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.rememberPending(id)
		gotID, behavior, ok := cs.permissionVerdict(slackReply("y " + id))
		if !ok || gotID != id || behavior != "allow" {
			t.Fatalf("want allow %q, got id=%q behavior=%q ok=%v", id, gotID, behavior, ok)
		}
	})

	t.Run("deny maps n", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.rememberPending(id)
		_, behavior, ok := cs.permissionVerdict(slackReply("no " + id))
		if !ok || behavior != "deny" {
			t.Fatalf("want deny, got behavior=%q ok=%v", behavior, ok)
		}
	})

	t.Run("uppercase id from issuer is matched case-insensitively", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.rememberPending("ABCDE")
		gotID, _, ok := cs.permissionVerdict(slackReply("y abcde"))
		if !ok || gotID != "abcde" {
			t.Fatalf("want canonical lowercase id, got id=%q ok=%v", gotID, ok)
		}
	})

	t.Run("forged: no Source is rejected even for a live nonce", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.rememberPending(id)
		// A content/system delivery (e.g. a published event) carries no Source.
		if _, _, ok := cs.permissionVerdict(protocol.MessagePayload{Text: "y " + id, Source: ""}); ok {
			t.Fatal("source-less message must not forge a verdict")
		}
		// The nonce must survive a rejected attempt so the real user can still answer.
		if _, _, ok := cs.permissionVerdict(slackReply("y " + id)); !ok {
			t.Fatal("nonce should remain answerable after a source-less attempt")
		}
	})

	t.Run("forged: an id the server never issued is rejected", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		if _, _, ok := cs.permissionVerdict(slackReply("y zzzzz")); ok {
			t.Fatal("an unissued id must not be accepted")
		}
	})

	t.Run("replay: a consumed nonce is rejected on second use", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.rememberPending(id)
		if _, _, ok := cs.permissionVerdict(slackReply("y " + id)); !ok {
			t.Fatal("first verdict should be honored")
		}
		if _, _, ok := cs.permissionVerdict(slackReply("y " + id)); ok {
			t.Fatal("a consumed nonce must not be honored again")
		}
	})

	t.Run("expired: a nonce past its TTL is rejected", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		base := time.Unix(0, 0)
		cs.now = func() time.Time { return base }
		cs.rememberPending(id)
		cs.now = func() time.Time { return base.Add(permissionTTL + time.Second) }
		if _, _, ok := cs.permissionVerdict(slackReply("y " + id)); ok {
			t.Fatal("an expired nonce must not be honored")
		}
	})

	t.Run("non-permission text is not a verdict", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.rememberPending(id)
		if _, _, ok := cs.permissionVerdict(slackReply("yes please do it")); ok {
			t.Fatal("free text must not be a verdict")
		}
	})

	t.Run("issuing a permission request remembers the nonce", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.handlePermissionRequest(context.Background(), newPermissionRequestNotification(id))
		if _, _, ok := cs.permissionVerdict(slackReply("y " + id)); !ok {
			t.Fatal("a reply to a just-issued request should be honored")
		}
	})

	// OnMessage must stay panic-free on both branches (no MCP clients attached).
	t.Run("OnMessage smoke", func(t *testing.T) {
		cs := NewChannelServer(&mockSender{})
		cs.rememberPending(id)
		cs.OnMessage(slackReply("y " + id))                        // verdict branch
		cs.OnMessage(protocol.MessagePayload{Text: "hello world"}) // forward branch
	})
}
