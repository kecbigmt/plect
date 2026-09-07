package service

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// Exercises Destroy end to end, not unifiedTeardownList directly.
func TestDestroy_ReleasesNodeRemovedFromWorkflow(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "kept", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "kept"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{
		"retired": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "retired", Seq: 1, Outputs: map[string]any{}},
		"kept":    {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Seq: 2, Outputs: map[string]any{}},
	})

	obs := &orderObserver{}
	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "sess-1", Observer: obs}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if idx(obs.cleaned, "retired") < 0 {
		t.Fatalf("cleaned = %v, want %q released even though the workflow no longer declares it", obs.cleaned, "retired")
	}
	if idx(obs.cleaned, "kept") < 0 {
		t.Fatalf("cleaned = %v, want %q released", obs.cleaned, "kept")
	}
}

// `plect down`'s counterpart: session-scoped state must survive.
func TestDown_ReleasesRunScopedNodeRemovedFromWorkflow(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "review", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "review"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{
		"retired": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "retired", Seq: 1, Outputs: map[string]any{}},
		"review":  {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "review", Seq: 2, Outputs: map[string]any{}},
	})

	obs := &orderObserver{}
	if _, err := Down(cfg, store, DownParams{Identifier: "sess-1", Observer: obs}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if idx(obs.cleaned, "retired") < 0 {
		t.Fatalf("cleaned = %v, want %q (run-scoped) released even though the workflow no longer declares it", obs.cleaned, "retired")
	}
	s := store.Get("sess-1")
	if s.Nodes["retired"] == nil || s.Nodes["retired"].Status != contract.TaskStatusCleaned {
		t.Errorf("retired should be persisted as cleaned: %+v", s.Nodes["retired"])
	}
	if s.Nodes["review"].Status != contract.TaskStatusProduced {
		t.Errorf("session-scoped review should survive down: %+v", s.Nodes["review"])
	}
}

// A later Destroy must release a failed/partial setup using its own
// retained cleanup contract alone, with no live task definition required.
func TestUp_FailedSetupRetainsCleanupContractAcrossARestart(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{
			id:      "flaky",
			scope:   "run",
			setup:   `echo 'partial allocation' >&2; exit 1`,
			cleanup: fmt.Sprintf(`printf 'flaky-cleaned\n' >> %s`, cleanupLog),
		}},
		[]nodeFixture{{id: "flaky"}},
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "sess-1", "acme", 1, "coding", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: "sess-1"}); err == nil {
		t.Fatal("Up: want the flaky setup's failure surfaced, got nil error")
	}

	s := store.Get("sess-1")
	if s == nil || s.Nodes["flaky"] == nil || s.Nodes["flaky"].Status != contract.TaskStatusFailed {
		t.Fatalf("after Up failure, flaky = %+v, want a retained failed attempt", s.Nodes["flaky"])
	}
	if len(s.Nodes["flaky"].Cleanup) == 0 {
		t.Fatal("after Up failure, flaky has no retained cleanup contract")
	}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "sess-1"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log: %v", err)
	}
	if !strings.Contains(string(data), "flaky-cleaned") {
		t.Fatalf("cleanup log = %q, want the retained cleanup contract to have run", data)
	}
}

func TestUnifiedTeardownList_ReleasesNodeRemovedFromCurrentWorkflow(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "kept", scope: "session", setup: "echo '{}'", cleanup: "true"}},
		[]nodeFixture{{id: "kept"}},
	)
	session := &domain.Session{
		Workflow: "wf",
		Nodes: map[string]*contract.TaskState{
			"retired": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "retired"},
			"kept":    {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "kept"},
		},
	}
	items, err := unifiedTeardownList(cfg, session, false)
	if err != nil {
		t.Fatalf("unifiedTeardownList: %v", err)
	}
	var sawRetired, sawKept bool
	for _, r := range items {
		switch r.NodeID {
		case "retired":
			sawRetired = true
		case "kept":
			sawKept = true
		}
	}
	if !sawRetired {
		t.Fatalf("items = %+v, want %q enumerated even though the workflow no longer declares it", items, "retired")
	}
	if !sawKept {
		t.Fatalf("items = %+v, want %q enumerated", items, "kept")
	}
}

func TestUnifiedTeardownList_UsesRetainedCleanupWhenCurrentDefinitionIsGone(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf", nil, nil)
	retained, err := json.Marshal(task.RetainedCleanup{Action: &lang.Action{Type: lang.ActionShell, Script: "retired-cleanup"}})
	if err != nil {
		t.Fatal(err)
	}
	session := &domain.Session{
		Workflow: "wf",
		Nodes: map[string]*contract.TaskState{
			"retired": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "retired", Cleanup: retained},
		},
	}
	items, err := unifiedTeardownList(cfg, session, false)
	if err != nil {
		t.Fatalf("unifiedTeardownList: %v", err)
	}
	if len(items) != 1 || items[0].Cleanup == nil || items[0].Cleanup.Script != "retired-cleanup" {
		t.Fatalf("items = %+v, want the retained cleanup action", items)
	}
}

// b's Seq is lower than its prerequisite a's, so RunCleanup's reverse pass
// must still release b first; that requires a to come out before b here.
func TestUnifiedTeardownList_OrdersByRecordedDependencyNotSeqAlone(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf", nil, nil)
	session := &domain.Session{
		Workflow: "wf",
		Nodes: map[string]*contract.TaskState{
			"a": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "a", Seq: 5},
			"b": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "b", Seq: 2, DependsOn: []string{"a"}},
		},
	}
	items, err := unifiedTeardownList(cfg, session, false)
	if err != nil {
		t.Fatalf("unifiedTeardownList: %v", err)
	}
	idxA, idxB := -1, -1
	for i, r := range items {
		switch r.NodeID {
		case "a":
			idxA = i
		case "b":
			idxB = i
		}
	}
	if idxA < 0 || idxB < 0 || idxA >= idxB {
		t.Fatalf("items = %+v, want %q (prerequisite) before %q (dependent) despite b.Seq < a.Seq, so RunCleanup's reverse pass releases %q first", items, "a", "b", "b")
	}
}
