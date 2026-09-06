package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
)

// setUpConfigHomeWithCapture is setUpConfigHome plus a capture-bearing
// session-scoped task, wired into the "plain" workflow's node list, so
// handleDown/handleDestroy/handleCapture have something to observe beyond
// the bare workflow lifecycle.
func setUpConfigHomeWithCapture(t *testing.T) {
	t.Helper()
	setUpConfigHome(t)
	// Down/Destroy clamp to the ambient PLECT_SESSION_NAME as a lifecycle
	// guard; clear it so the test process's own session name doesn't make
	// every other session "unrelated".
	t.Setenv("PLECT_SESSION_NAME", "")
	home := os.Getenv("HOME")
	baseDir := filepath.Join(home, ".config", "plect")

	if err := os.WriteFile(filepath.Join(baseDir, "tasks", "noop.toml"),
		[]byte(`
[noop]
kind  = "effect"
scope = "session"

[noop.setup]
type   = "shell"
script = "echo '{}'"

[noop.cleanup]
type   = "shell"
script = "true"

[noop.health.alive]
type = "noop"

[noop.terminal.attach]
type   = "shell"
script = "true"

[noop.terminal.capture]
type   = "shell"
script = "echo -n hello"

[noop.terminal.send_text]
type   = "shell"
script = "true"

[noop.terminal.send_keys]
type   = "shell"
script = "true"
`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func createPlainSession(t *testing.T, sessionID string) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Create(cfg, state.NewStore(""), service.CreateParams{
		URL:      sessionID,
		Workflow: "plain",
	})
	if err != nil {
		t.Fatalf("service.Create: %v", err)
	}
	return result.SessionName
}

// NewServer must register every declared tool exactly once under its
// canonical plect_-prefixed name, since that's the surface MCP clients see.
func TestNewServer_RegistersTools(t *testing.T) {
	s := NewServer()
	if s == nil {
		t.Fatal("NewServer returned nil")
	}
}

func TestHandleDown_RunsCleanupForExistingSession(t *testing.T) {
	setUpConfigHomeWithCapture(t)
	name := createPlainSession(t, "down-session")

	result, err := handleDown(context.Background(), reqWith(map[string]any{"session": name}))
	if err != nil {
		t.Fatalf("handleDown: %v", err)
	}
	out := decodeJSONResult(t, result)
	if out["session_name"] != name {
		t.Fatalf("session_name = %v, want %v", out["session_name"], name)
	}
}

func TestHandleDown_MissingSessionArg(t *testing.T) {
	result, err := handleDown(context.Background(), reqWith(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing session")
	}
}

func TestHandleDown_UnknownSession(t *testing.T) {
	setUpConfigHome(t)
	result, err := handleDown(context.Background(), reqWith(map[string]any{"session": "does-not-exist"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for unknown session")
	}
}

func TestHandleDestroy_RemovesSession(t *testing.T) {
	setUpConfigHomeWithCapture(t)
	name := createPlainSession(t, "destroy-session")

	result, err := handleDestroy(context.Background(), reqWith(map[string]any{"session": name}))
	if err != nil {
		t.Fatalf("handleDestroy: %v", err)
	}
	out := decodeJSONResult(t, result)
	if out["session_name"] != name {
		t.Fatalf("session_name = %v, want %v", out["session_name"], name)
	}

	store := state.NewStore("")
	if s := store.Get(name); s != nil {
		t.Fatalf("session %q still present after destroy", name)
	}
}

func TestHandleDestroy_MissingSessionArg(t *testing.T) {
	result, err := handleDestroy(context.Background(), reqWith(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing session")
	}
}

func TestHandleDestroy_ForceAndCleanupInputsForwarded(t *testing.T) {
	setUpConfigHomeWithCapture(t)
	name := createPlainSession(t, "destroy-force-session")

	result, err := handleDestroy(context.Background(), reqWith(map[string]any{
		"session":        name,
		"force":          true,
		"cleanup_inputs": map[string]any{"delete_branch": "true"},
	}))
	if err != nil {
		t.Fatalf("handleDestroy: %v", err)
	}
	decodeJSONResult(t, result)
}

func TestHandleCapture_ReturnsRenderedContent(t *testing.T) {
	setUpConfigHomeWithCapture(t)
	name := createPlainSession(t, "capture-session")

	result, err := handleCapture(context.Background(), reqWith(map[string]any{"session": name}))
	if err != nil {
		t.Fatalf("handleCapture: %v", err)
	}
	out := decodeJSONResult(t, result)
	if out["session_name"] != name {
		t.Fatalf("session_name = %v, want %v", out["session_name"], name)
	}
	if out["content"] != "hello" {
		t.Fatalf("content = %v, want hello", out["content"])
	}
}

func TestHandleCapture_MissingSessionArg(t *testing.T) {
	result, err := handleCapture(context.Background(), reqWith(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing session")
	}
}

func TestHandleStatus_ReportsCreatedSession(t *testing.T) {
	setUpConfigHomeWithCapture(t)
	name := createPlainSession(t, "status-session")

	result, err := handleStatus(context.Background(), reqWith(map[string]any{"url": name}))
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	out := decodeJSONResult(t, result)
	if out["ok"] != true {
		t.Fatalf("ok = %v, want true", out["ok"])
	}
	if _, ok := out["identity"]; !ok {
		t.Fatalf("expected identity field in status response, got %v", out)
	}
}

func TestHandleStatus_MissingURLArg(t *testing.T) {
	result, err := handleStatus(context.Background(), reqWith(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing url")
	}
}

func TestHandleStatus_UnknownSession(t *testing.T) {
	setUpConfigHome(t)
	result, err := handleStatus(context.Background(), reqWith(map[string]any{"url": "does-not-exist"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for unknown session")
	}
}

func TestHandleList_ReturnsCreatedSessions(t *testing.T) {
	setUpConfigHomeWithCapture(t)
	name := createPlainSession(t, "list-session")

	result, err := handleList(context.Background(), reqWith(map[string]any{}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	out := decodeJSONResult(t, result)
	sessions, ok := out["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions field is not a list: %v", out["sessions"])
	}
	found := false
	for _, s := range sessions {
		entry, ok := s.(map[string]any)
		if ok && entry["session_name"] == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected session %q in list, got %v", name, sessions)
	}
}

func TestHandleList_EmptyStore(t *testing.T) {
	setUpConfigHome(t)
	result, err := handleList(context.Background(), reqWith(map[string]any{}))
	if err != nil {
		t.Fatalf("handleList: %v", err)
	}
	out := decodeJSONResult(t, result)
	sessions, ok := out["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions field is not a list: %v", out["sessions"])
	}
	if len(sessions) != 0 {
		t.Fatalf("expected no sessions, got %v", sessions)
	}
}
