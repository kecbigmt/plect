package config

import (
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/domain"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// writeRunScopedWorkflow writes a workflow "default" declaring two run-scoped
// effect nodes, "pane" and "agent", under a fresh base dir.
func writeRunScopedWorkflow(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "workflows", "default.toml"), `
[default]
kind = "workflow"

[[default.nodes]]
id   = "pane"
uses = "pane"

[[default.nodes]]
id   = "agent"
uses = "agent"
`)
	for _, id := range []string{"pane", "agent"} {
		writeTaskFile(t, base, id, `
[`+id+`]
kind  = "effect"
scope = "run"
`)
	}
	return base
}

func TestConfig_RunScopeUp_NoWorkflowFallsBackToAnyProducedEntry(t *testing.T) {
	cases := []struct {
		name  string
		tasks map[string]*contract.TaskState
		want  bool
	}{
		{"nil tasks", nil, false},
		{"only session-scoped produced", map[string]*contract.TaskState{
			"slack_thread": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
		}, false},
		{"run-scoped cleaned", map[string]*contract.TaskState{
			"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusCleaned},
		}, false},
		{"run-scoped produced", map[string]*contract.TaskState{
			"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		}, true},
	}
	cfg := &Config{}
	for _, c := range cases {
		s := &domain.Session{Tasks: c.tasks}
		if got := cfg.RunScopeUp(s); got != c.want {
			t.Errorf("%s: RunScopeUp = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConfig_RunScopeUp_StaleProducedNodeAloneIsDown(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	cfg := &Config{BaseDir: base}
	s := &domain.Session{
		Workflow: "default",
		Tasks: map[string]*contract.TaskState{
			// "removed_node" is a stale record: the workflow above declares
			// only "pane" and "agent".
			"removed_node": {Scope: contract.TaskScopeRun, TaskID: "removed_node", Status: contract.TaskStatusProduced},
		},
	}
	if cfg.RunScopeUp(s) {
		t.Fatal("RunScopeUp = true, want false (the only produced entry is for a node the workflow no longer declares)")
	}
}

func TestConfig_RunScopeUp_ProducedCurrentPlanNodeIsUp(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	cfg := &Config{BaseDir: base}
	s := &domain.Session{
		Workflow: "default",
		Tasks: map[string]*contract.TaskState{
			"pane": {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
		},
	}
	if !cfg.RunScopeUp(s) {
		t.Fatal("RunScopeUp = false, want true (pane is a current-plan run-scoped node and is produced)")
	}
}

func TestConfig_RunScopeUp_UnresolvableWorkflowFallsBackToAnyProducedEntry(t *testing.T) {
	base := writeRunScopedWorkflow(t)
	cfg := &Config{BaseDir: base}
	s := &domain.Session{
		Workflow: "ghost-workflow",
		Tasks: map[string]*contract.TaskState{
			"pane": {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
		},
	}
	if !cfg.RunScopeUp(s) {
		t.Fatal("RunScopeUp = false, want true (an unresolvable workflow falls back rather than misreporting down)")
	}
}
