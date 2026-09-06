package service

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
)

func writeTaskFile(t *testing.T, base, id, body string) {
	t.Helper()
	dir := filepath.Join(base, "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTaskShow_ReportsNestingChainOutermostFirst covers the static nesting
// view: every layer of the chain, in the order setup runs them.
func TestTaskShow_ReportsNestingChainOutermostFirst(t *testing.T) {
	base := t.TempDir()
	writeTaskFile(t, base, "claude", `
[claude]
kind = "effect"
scope = "session"

[claude.setup]
type   = "shell"
script = "true"

[claude.health.alive]
type = "noop"
`)
	writeTaskFile(t, base, "guarded", `
[guarded]
kind = "effect"

[guarded.inner]
uses = "claude"
`)
	writeTaskFile(t, base, "team", `
[team]
kind = "effect"

[team.inner]
uses = "guarded"
`)

	detail, err := TaskShow(&config.Config{BaseDir: base}, "", "team")
	if err != nil {
		t.Fatalf("TaskShow: %v", err)
	}
	if detail.Scope != "session" {
		t.Errorf("Scope = %q, want the innermost task's %q", detail.Scope, "session")
	}
	var got []string
	for _, layer := range detail.Nesting {
		got = append(got, layer.ID)
	}
	want := []string{"team", "guarded", "claude"}
	if len(got) != len(want) {
		t.Fatalf("Nesting = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Nesting = %v, want %v", got, want)
		}
	}
	if detail.Nesting[0].Inner != "claude" && detail.Nesting[0].Inner != "guarded" {
		t.Errorf("outermost layer inner = %q, want the reference as written", detail.Nesting[0].Inner)
	}
}

// TestTaskShow_PlainTaskReportsNoChain keeps the nesting view out of the way
// of the tasks that are not nested at all.
func TestTaskShow_PlainTaskReportsNoChain(t *testing.T) {
	base := t.TempDir()
	writeTaskFile(t, base, "claude", `
[claude]
kind = "effect"
scope = "run"

[claude.setup]
type   = "shell"
script = "true"

[claude.health.alive]
type = "noop"
`)
	detail, err := TaskShow(&config.Config{BaseDir: base}, "", "claude")
	if err != nil {
		t.Fatalf("TaskShow: %v", err)
	}
	if len(detail.Nesting) != 0 {
		t.Errorf("Nesting = %v, want empty for a plain task", detail.Nesting)
	}
}

func TestTaskShow_UnknownTask(t *testing.T) {
	base := t.TempDir()
	if _, err := TaskShow(&config.Config{BaseDir: base}, "", "nope"); err == nil {
		t.Fatal("TaskShow: want an error for an unknown task id, got nil")
	}
}

func TestTaskShow_ReportsExtendsChainWithPerLayerProvenance(t *testing.T) {
	base := t.TempDir()
	writeTaskFile(t, base, "resources", `
[issue_pr]
kind  = "resource_observer"
match = '^https://github\.com/'

[issue_pr.observe]
type    = "exec"
command = "true"

[issue_pr.state_schema]
type = "object"

[issue_pr.state_schema.properties]
checks_status = { type = "string" }
`)
	writeTaskFile(t, base, "work", `
[work]
kind              = "task"
resource_observer = "issue_pr"
instructions      = [{ text = "Resolve the issue." }]

[work.done_when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge = "acceptance criteria are satisfied", id = "ac-met" },
]

[work_claude]
kind         = "task"
extends      = "work"
instructions = [
  { text = "Use the claude runtime." },
  { text = "Prefer opus for review." },
]

[[work_claude.chains]]
id        = "review"
workflow  = "claude_reviewer"
placement = "sibling"

[work_claude.chains.when]
all = [{ judge_pending = "ac-met" }]

[work_claude.state_schema]
type = "object"

[work_claude.state_schema.properties]
model = { type = "string", default = "opus" }
`)
	writeTaskFile(t, base, "claude_reviewer", `
[claude_reviewer]
kind = "workflow"
`)

	detail, err := TaskShow(&config.Config{BaseDir: base}, "", "work_claude")
	if err != nil {
		t.Fatalf("TaskShow: %v", err)
	}
	if len(detail.ExtendsChain) != 2 {
		t.Fatalf("ExtendsChain = %+v, want two layers", detail.ExtendsChain)
	}
	outer, inner := detail.ExtendsChain[0], detail.ExtendsChain[1]
	if outer.ID != "work_claude" || inner.ID != "work" {
		t.Fatalf("ExtendsChain order = [%s, %s], want outermost (work_claude) first", outer.ID, inner.ID)
	}
	wantOuterInstructions := []string{"Use the claude runtime.", "Prefer opus for review."}
	if !slices.Equal(outer.Instructions, wantOuterInstructions) {
		t.Errorf("outer.Instructions = %v, want %v (each element attributed separately)", outer.Instructions, wantOuterInstructions)
	}
	if len(outer.Chains) != 1 || outer.Chains[0] != "review" {
		t.Errorf("outer.Chains = %v, want [review], attributed to work_claude", outer.Chains)
	}
	if len(outer.DoneWhen) != 0 {
		t.Errorf("outer.DoneWhen = %v, want none: work_claude declares no done_when of its own", outer.DoneWhen)
	}
	if want := []string{"model (default)"}; !slices.Equal(outer.StateSchemaKeys, want) {
		t.Errorf("outer.StateSchemaKeys = %v, want %v", outer.StateSchemaKeys, want)
	}
	if inner.Instructions[0] != "Resolve the issue." {
		t.Errorf("inner.Instructions = %v, want only work's own segment", inner.Instructions)
	}
	wantInnerDoneWhen := []string{"check resource.state.checks_status", "judge ac-met"}
	if !slices.Equal(inner.DoneWhen, wantInnerDoneWhen) {
		t.Errorf("inner.DoneWhen = %v, want %v (every leaf kind, not only judges)", inner.DoneWhen, wantInnerDoneWhen)
	}
}
