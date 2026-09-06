package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/mcpserver"
	"github.com/kecbigmt/plecture/app/internal/service"
)

// helperProcessEnv is the sentinel that makes this test binary double as the
// `self` executable mcpserver.Listen re-execs as "plect mcp serve" — go test
// has no separate plect binary to point mcpListenCmd's os.Executable() lookup
// at, so the test binary re-execs itself (the same helper-process pattern
// os/exec_test.go uses).
const helperProcessEnv = "PLECT_MCP_LISTEN_TEST_HELPER_PROCESS"

func TestMain(m *testing.M) {
	if os.Getenv(helperProcessEnv) == "1" {
		rootCmd.SetArgs(os.Args[1:])
		if err := rootCmd.Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// PLECT_CONFIG_HOME and XDG_CONFIG_HOME both outrank HOME in
	// confighome.Resolve()'s precedence, so left ambient either would bypass
	// every test's HOME-based isolation below.
	os.Unsetenv(confighome.EnvVar)
	os.Unsetenv(confighome.XDGEnvVar)
	os.Exit(m.Run())
}

// mcpTestClient hand-rolls the client side since no library transport wraps
// an arbitrary net.Conn for MCP's newline-delimited JSON-RPC framing.
type mcpTestClient struct {
	conn   net.Conn
	reader *bufio.Reader
	nextID int
}

func newMCPTestClient(t *testing.T, socketPath string) *mcpTestClient {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %s: %v", socketPath, err)
	}
	t.Cleanup(func() { conn.Close() })
	return &mcpTestClient{conn: conn, reader: bufio.NewReader(conn)}
}

func (c *mcpTestClient) request(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	c.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params}
	line, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		t.Fatalf("write %s request: %v", method, err)
	}
	respLine, err := c.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		t.Fatalf("decode %s response %q: %v", method, respLine, err)
	}
	return resp
}

func (c *mcpTestClient) initialize(t *testing.T) {
	t.Helper()
	c.request(t, "initialize", map[string]any{
		"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-listen-guard-test", "version": "0.0.1"},
	})
}

// callTool returns the decoded JSON body of the tool's text content, whether
// the call succeeded (jsonResult) or was rejected (errorResult) — both
// mcpserver helpers report their outcome as a JSON text content item rather
// than a transport-level JSON-RPC error.
func (c *mcpTestClient) callTool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	resp := c.request(t, "tools/call", map[string]any{"name": name, "arguments": args})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call response has no result: %v", resp)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call result has no content: %v", result)
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected content item: %v", content[0])
	}
	text, _ := item["text"].(string)
	var body map[string]any
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode tool result text %q: %v", text, err)
	}
	return body
}

// TestMCPListen_ScopesSessionGuardToOwnSession runs the listener with no
// PLECT_SESSION_GUARD of its own, so a rejection can only come from the
// injected per-connection guard.
func TestMCPListen_ScopesSessionGuardToOwnSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv(helperProcessEnv, "1")

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	socketPath := filepath.Join(t.TempDir(), "listen.sock")
	guard, err := service.SessionGuardForOwnSession("ownerA/session-a")
	if err != nil {
		t.Fatalf("SessionGuardForOwnSession: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	listenErr := make(chan error, 1)
	go func() { listenErr <- mcpserver.Listen(ctx, socketPath, self, guard) }()
	waitForSocket(t, socketPath)
	t.Cleanup(func() {
		cancel()
		if err := <-listenErr; err != nil {
			t.Errorf("Listen: %v", err)
		}
	})

	connect := func() *mcpTestClient {
		t.Helper()
		client := newMCPTestClient(t, socketPath)
		client.initialize(t)
		return client
	}

	t.Run("in scope: own session", func(t *testing.T) {
		body := connect().callTool(t, "plect_event_publish", map[string]any{
			"session": "ownerA/session-a",
			"type":    "test.probe",
		})
		if ok, _ := body["ok"].(bool); !ok {
			t.Errorf("in-scope publish rejected: %v", body)
		}
	})

	t.Run("in scope: own subtree", func(t *testing.T) {
		body := connect().callTool(t, "plect_event_publish", map[string]any{
			"session": "ownerA/session-a/child",
			"type":    "test.probe",
		})
		if ok, _ := body["ok"].(bool); !ok {
			t.Errorf("in-subtree publish rejected: %v", body)
		}
	})

	t.Run("out of scope: sibling owner", func(t *testing.T) {
		body := connect().callTool(t, "plect_event_publish", map[string]any{
			"session": "ownerB/other-session",
			"type":    "test.probe",
		})
		if ok, _ := body["ok"].(bool); ok {
			t.Fatalf("out-of-scope publish was not rejected: %v", body)
		}
		if code, _ := body["code"].(string); code != "repo_not_allowed" {
			t.Errorf("code = %v, want repo_not_allowed: %v", body["code"], body)
		}
	})
}

// waitForSocket polls until mcpserver.Listen has created the socket file, so
// the first dial doesn't race the listener goroutine's startup.
func waitForSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s was not created in time", socketPath)
}
