package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/sockettest"
)

func TestListenSetsSocketPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not available on windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := filepath.Join(sockettest.Dir(t), "plect.sock")
	done := runTestListener(t, ctx, socket)
	waitForSocketMode(t, socket, 0o666)

	cancel()
	waitForListener(t, done)
}

func TestListenAcceptsConcurrentConnections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not available on windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := filepath.Join(sockettest.Dir(t), "plect.sock")
	done := runTestListener(t, ctx, socket)
	waitForSocketMode(t, socket, 0o666)

	const clients = 4
	start := make(chan struct{})
	pids := make(chan int, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			pid, err := callHelperMCP(socket, fmt.Sprintf("client-%d", i))
			if err != nil {
				errs <- err
				return
			}
			pids <- pid
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	close(pids)

	for err := range errs {
		t.Error(err)
	}
	seen := map[int]bool{}
	for pid := range pids {
		seen[pid] = true
	}
	if len(seen) != clients {
		t.Fatalf("got %d child processes, want %d: %v", len(seen), clients, seen)
	}

	cancel()
	waitForListener(t, done)
}

func TestListenReapsChildAfterDisconnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are not available on windows")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socket := filepath.Join(sockettest.Dir(t), "plect.sock")
	done := runTestListener(t, ctx, socket)
	waitForSocketMode(t, socket, 0o666)

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	pid, err := callHelperMCPOnConn(conn, "cleanup")
	if err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	waitForProcessGone(t, pid)

	cancel()
	waitForListener(t, done)
}

func TestMCPListenHelperProcess(t *testing.T) {
	if os.Getenv("PLECT_MCP_LISTEN_HELPER") != "1" {
		return
	}
	serveHelperMCP()
	os.Exit(0)
}

func runTestListener(t *testing.T, ctx context.Context, socket string) <-chan error {
	t.Helper()
	self := writeHelperExecutable(t)
	done := make(chan error, 1)
	go func() {
		done <- Listen(ctx, socket, self, "")
	}()
	return done
}

func writeHelperExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plect-helper")
	script := fmt.Sprintf("#!/bin/sh\nPLECT_MCP_LISTEN_HELPER=1 exec %q -test.run=TestMCPListenHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	return path
}

func waitForSocketMode(t *testing.T, socket string, want os.FileMode) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info, err := os.Stat(socket)
		if err == nil && info.Mode().Perm() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("socket was not created: %v", err)
	}
	t.Fatalf("socket mode = %o, want %o", info.Mode().Perm(), want)
}

func waitForListener(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("listener: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not stop")
	}
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return
		}
		if err != nil && err != syscall.EPERM {
			t.Fatalf("checking process %d: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d still exists after client disconnect", pid)
}

func callHelperMCP(socket, name string) (int, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return 0, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	return callHelperMCPOnConn(conn, name)
}

func callHelperMCPOnConn(conn net.Conn, name string) (int, error) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	}); err != nil {
		return 0, fmt.Errorf("initialize request: %w", err)
	}
	if _, err := readRPCResponse(dec); err != nil {
		return 0, fmt.Errorf("initialize response: %w", err)
	}
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "pid",
			"arguments": map[string]any{"name": name},
		},
	}); err != nil {
		return 0, fmt.Errorf("tool request: %w", err)
	}
	resp, err := readRPCResponse(dec)
	if err != nil {
		return 0, fmt.Errorf("tool response: %w", err)
	}
	content, ok := resp.Result["content"].([]any)
	if !ok || len(content) == 0 {
		return 0, fmt.Errorf("missing content in response: %#v", resp.Result)
	}
	text, ok := content[0].(map[string]any)["text"].(string)
	if !ok {
		return 0, fmt.Errorf("missing text content: %#v", content[0])
	}
	var body struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		return 0, fmt.Errorf("decode text content: %w", err)
	}
	return body.PID, nil
}

type rpcResponse struct {
	Result map[string]any `json:"result"`
	Error  any            `json:"error"`
}

func readRPCResponse(dec *json.Decoder) (rpcResponse, error) {
	var resp rpcResponse
	if err := dec.Decode(&resp); err != nil {
		return resp, err
	}
	if resp.Error != nil {
		return resp, fmt.Errorf("rpc error: %v", resp.Error)
	}
	return resp, nil
}

func serveHelperMCP() {
	args := []string{}
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		if req.ID == 0 {
			continue
		}
		switch req.Method {
		case "initialize":
			enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "helper", "version": "0"},
				},
			})
		case "tools/call":
			text, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "args": args})
			enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": string(text)}},
				},
			})
		default:
			enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found: " + strconv.Quote(req.Method)},
			})
		}
	}
}
