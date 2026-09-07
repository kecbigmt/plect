package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/kecbigmt/plecture/app/internal/state"
)

// setUpConfigHome points config.Load()/state.NewStore("") at a scratch config
// home with a resolver-less workflow, so dispatch treats the identifier as the
// session id directly.
func setUpConfigHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	baseDir := filepath.Join(home, ".config", "plect")
	for _, dir := range []string{"workflows", "tasks", "workspaces"} {
		if err := os.MkdirAll(filepath.Join(baseDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(baseDir, "config.toml"),
		[]byte("schema_version = 2\nworkspace_dirs_root = \""+filepath.Join(home, "workspace_dirs")+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceDirPath := filepath.Join(home, "wd")
	if err := os.WriteFile(filepath.Join(baseDir, "workspaces", "plain_provider.toml"),
		[]byte(providerDocument("plain_provider", workspaceDirPath)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "workflows", "plain.toml"),
		[]byte("[plain]\nkind = \"workflow\"\nworkspace_provider = \"plain_provider\"\n\n[[plain.nodes]]\nuses = \"noop\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "tasks", "noop.toml"),
		[]byte("[noop]\nkind = \"effect\"\nscope = \"session\"\n\n[noop.setup]\ntype = \"shell\"\nscript = \"echo '{}'\"\n\n[noop.health.alive]\ntype = \"noop\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decodeJSONResult(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected error result: %s", extractErrorText(result))
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", result.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return out
}

func TestNewServer_RemovesRemovedTools(t *testing.T) {
	tools := NewServer().ListTools()
	for _, name := range []string{"plect_create", "plect_watchdog_check"} {
		if _, ok := tools[name]; ok {
			t.Fatalf("%s is still registered", name)
		}
	}
	for _, name := range []string{"plect_up", "plect_down", "plect_destroy"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("%s is not registered", name)
		}
	}
}

// A resolver-less workflow's identifier isn't a URL, so plect_up's
// docker-compose-up-style auto-create only kicks in for resolver-matched or
// URL identifiers. Once a resolver-less session exists, plect_up must still
// resolve and run it.
func TestHandleUp_ResolverLessWorkflowExistingSession(t *testing.T) {
	setUpConfigHome(t)

	createPlainSession(t, "my-session")

	result, err := handleUp(context.Background(), reqWith(map[string]any{
		"session":  "my-session+plain",
		"workflow": "plain",
	}))
	if err != nil {
		t.Fatalf("handleUp: %v", err)
	}
	out := decodeJSONResult(t, result)
	if out["session_name"] != "my-session+plain" {
		t.Fatalf("session_name = %v, want my-session+plain", out["session_name"])
	}

	store := state.NewStore("")
	s := store.Get("my-session+plain")
	if s == nil {
		t.Fatal("session not persisted")
	}
	if s.Workflow != "plain" {
		t.Errorf("Workflow = %q, want plain", s.Workflow)
	}
}

// The tool schemas must expose workflow/task so a client can discover the
// arguments without reading the handler source.
func TestUpTool_ExposesWorkflowAndTask(t *testing.T) {
	for _, name := range []string{"workflow", "task"} {
		if _, ok := upTool.InputSchema.Properties[name]; !ok {
			t.Errorf("%s: missing %q in InputSchema.Properties", upTool.Name, name)
		}
	}
}

// A task value conflicting with inputs.task must fail rather than silently
// picking one — same rule the CLI/Web UI shorthand follows.
func TestHandleUp_TaskConflictsWithInputs(t *testing.T) {
	setUpConfigHome(t)

	result, err := handleUp(context.Background(), reqWith(map[string]any{
		"session":  "my-session",
		"workflow": "plain",
		"task":     "review",
		"inputs":   map[string]any{"task": "work"},
	}))
	if err != nil {
		t.Fatalf("handleUp: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for conflicting task")
	}
}

// providerDocument is a workspace provider whose setup creates the workspace
// directory it reports, which is all these lifecycle tests need from one.
func providerDocument(id, workspaceDir string) string {
	return fmt.Sprintf(`
[%[1]s]
kind = "workspace_provider"

[%[1]s.setup]
type    = "exec"
command = "sh"
args = [
  "-c",
  """mkdir -p "$1" && printf '{\"workspace_dir\":\"%%s\"}' "$1\"""",
  "provider",
  "%[2]s",
]

[%[1]s.health.alive]
type = "noop"
`, id, workspaceDir)
}
