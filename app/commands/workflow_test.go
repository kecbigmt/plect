package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/service"
)

func writeWorkflowShowFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setUpBrokenWorkspaceProviderFixture(t *testing.T) {
	t.Helper()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	globalDir := filepath.Join(fakeHome, ".config", "plect")
	writeWorkflowShowFixtureFile(t, filepath.Join(globalDir, "config.toml"), "schema_version = 2\n")
	writeWorkflowShowFixtureFile(t, filepath.Join(globalDir, "tasks", "tmux.toml"), `
[tmux]
kind  = "effect"
scope = "run"

[tmux.setup]
type   = "shell"
script = "echo '{}'"

[tmux.health.alive]
type = "noop"
`)
	writeWorkflowShowFixtureFile(t, filepath.Join(globalDir, "workspaces", "broken.toml"), `
[broken]
kind = "workspace_provider"
`)
	writeWorkflowShowFixtureFile(t, filepath.Join(globalDir, "workflows", "usesbroken.toml"), `
[usesbroken]
kind = "workflow"
workspace_provider = "broken"

[[usesbroken.nodes]]
uses = "tmux"
`)
}

func TestWriteWorkflowList_NoHeaderUsesLiteralTabs(t *testing.T) {
	var buf bytes.Buffer
	err := writeWorkflowList(&buf, []service.WorkflowSummary{
		{ID: "bare"},
		{ID: "coding-claude", Name: "Coding agent", Description: "desc"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	wantLines := []string{
		"bare\t\t\n",
		"coding-claude\tCoding agent\tdesc\n",
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("expected %q in output; got:\n%s", line, got)
		}
	}
	if strings.Contains(got, "ID\tNAME\tDESCRIPTION") {
		t.Errorf("--no-header output should not contain the header row; got:\n%s", got)
	}
}

func TestWriteNodesByScope_GroupsAndPreservesOrder(t *testing.T) {
	var buf bytes.Buffer
	writeNodesByScope(&buf, []service.WorkflowNode{
		{ID: "envfile", Uses: "envfile", Scope: "session"},
		{ID: "slack_thread", Uses: "slack_thread", Scope: "session"},
		{ID: "tmux", Uses: "tmux", Scope: "run"},
		{ID: "claude", Uses: "claude", Scope: "run"},
	})
	got := buf.String()
	want := "  session:\n    envfile\n    slack_thread\n  run:\n    tmux\n    claude\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteNodesByScope_AnnotatesAliasUses(t *testing.T) {
	var buf bytes.Buffer
	writeNodesByScope(&buf, []service.WorkflowNode{
		{ID: "claude_second", Uses: "claude", Scope: "run"},
	})
	got := buf.String()
	if !strings.Contains(got, "claude_second (uses claude)") {
		t.Errorf("expected `uses` annotation for aliased node; got:\n%s", got)
	}
}

func TestWriteChannels_ShowsTypeAndDelivers(t *testing.T) {
	var buf bytes.Buffer
	writeChannels(&buf, []service.WorkflowChannel{
		{Name: "runtime", Uses: "claude_channel", Type: "unix_socket", Include: []string{"plect.instruction", "github.*"}},
	})
	got := buf.String()
	want := "  runtime (uses claude_channel, unix_socket)\n    delivers: plect.instruction, github.*\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteChannels_OmitsBlankTypeAndDelivers(t *testing.T) {
	var buf bytes.Buffer
	writeChannels(&buf, []service.WorkflowChannel{
		{Name: "runtime", Uses: "claude_channel"},
	})
	got := buf.String()
	want := "  runtime (uses claude_channel)\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestWritePopulationMembers_ShowsLastErrorAndStreak(t *testing.T) {
	var buf bytes.Buffer
	err := writePopulationMembers(&buf, []service.PopulationMemberStatus{
		{Resource: "urn:case:a", Session: "a+agent", PendingUp: true,
			LastAdmitReason: "input", LastAdmitError: "resolved to nothing", ConsecutiveAdmitFailures: 3},
		{Resource: "urn:case:b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(line, "urn:case:a"):
			for _, want := range []string{"a+agent", "true", "input: resolved to nothing", "3"} {
				if !strings.Contains(line, want) {
					t.Errorf("row a missing %q; got:\n%s", want, line)
				}
			}
		case strings.HasPrefix(line, "urn:case:b"):
			for _, want := range []string{"-", "false"} {
				if !strings.Contains(line, want) {
					t.Errorf("row b missing %q; got:\n%s", want, line)
				}
			}
		}
	}
	if !strings.Contains(got, "RESOURCE") {
		t.Errorf("expected a header row; got:\n%s", got)
	}
}

func TestWorkflowShow_WorkspaceProviderLoadErrorExitsNonzero(t *testing.T) {
	setUpBrokenWorkspaceProviderFixture(t)

	out, err := execRoot(t, "workflow", "show", "usesbroken")
	if err == nil {
		t.Fatalf("expected nonzero exit when the referenced workspace provider fails to load; output:\n%s", out)
	}
	if !strings.Contains(out, "usesbroken") {
		t.Errorf("output should still render the rest of the workflow; got:\n%s", out)
	}
	if !strings.Contains(out, "broken") {
		t.Errorf("output should name the failing definition; got:\n%s", out)
	}
	if !strings.Contains(out, "setup") {
		t.Errorf("output should name the load diagnostic; got:\n%s", out)
	}
}

func TestWorkflowShow_JSONWorkspaceProviderLoadErrorExitsNonzeroAndKeepsDetail(t *testing.T) {
	t.Cleanup(func() { workflowShowJSON = false })
	setUpBrokenWorkspaceProviderFixture(t)

	out, err := execRoot(t, "workflow", "show", "--json", "usesbroken")
	if err == nil {
		t.Fatalf("expected nonzero exit for --json output too; output:\n%s", out)
	}
	var detail service.WorkflowDetail
	if jsonErr := json.Unmarshal([]byte(out), &detail); jsonErr != nil {
		t.Fatalf("--json output is not valid JSON: %v; got:\n%s", jsonErr, out)
	}
	if detail.WorkspaceProviderError == "" {
		t.Fatal("expected workspace_provider_error to be populated in JSON output")
	}
	if !strings.Contains(detail.WorkspaceProviderError, "broken") {
		t.Errorf("workspace_provider_error should name the failing definition; got %q", detail.WorkspaceProviderError)
	}
	if !strings.Contains(detail.WorkspaceProviderError, "setup") {
		t.Errorf("workspace_provider_error should name the setup diagnostic; got %q", detail.WorkspaceProviderError)
	}
	if len(detail.Nodes) != 1 || detail.Nodes[0].ID != "tmux" {
		t.Errorf("expected the rest of the workflow detail to be preserved; got nodes=%+v", detail.Nodes)
	}
}

func TestWriteWorkflowList_HumanFillsBlankSentinels(t *testing.T) {
	var buf bytes.Buffer
	err := writeWorkflowList(&buf, []service.WorkflowSummary{
		{ID: "bare"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "(no name)") {
		t.Errorf("expected (no name) sentinel; got:\n%s", got)
	}
	if !strings.Contains(got, "(no description)") {
		t.Errorf("expected (no description) sentinel; got:\n%s", got)
	}
}
