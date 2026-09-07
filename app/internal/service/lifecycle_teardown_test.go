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

// TestDestroy_ReleasesNodeRemovedFromWorkflow proves Destroy itself (not
// just plect up's own separate stale-cleanup path) tears down a produced
// node the current workflow no longer declares, exercised end to end
// through the real Destroy call rather than unifiedTeardownList directly.
func TestDestroy_ReleasesNodeRemovedFromWorkflow(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "kept", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "kept"}}, // "retired" is no longer declared
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{
		"retired": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "retired", Seq: 1, Outputs: map[string]any{}},
		"kept":    {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Seq: 2, Outputs: map[string]any{}},
	})

	obs := &orderObserver{}
	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "o/r-1", Observer: obs}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if idx(obs.cleaned, "retired") < 0 {
		t.Fatalf("cleaned = %v, want %q released even though the workflow no longer declares it", obs.cleaned, "retired")
	}
	if idx(obs.cleaned, "kept") < 0 {
		t.Fatalf("cleaned = %v, want %q released", obs.cleaned, "kept")
	}
}

// TestDown_ReleasesRunScopedNodeRemovedFromWorkflow is TestDestroy_ReleasesNodeRemovedFromWorkflow's
// counterpart for `plect down`: a run-scoped node the current workflow no
// longer declares is still released, and the session's other (session-scoped)
// state survives, matching Down's existing run-only scoping.
func TestDown_ReleasesRunScopedNodeRemovedFromWorkflow(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "review", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "review"}}, // "retired" is no longer declared
	)
	store := testStore(t)
	seedSessionWithNodes(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{
		"retired": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, TaskID: "retired", Seq: 1, Outputs: map[string]any{}},
		"review":  {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "review", Seq: 2, Outputs: map[string]any{}},
	})

	obs := &orderObserver{}
	if _, err := Down(cfg, store, DownParams{Identifier: "o/r-1", Observer: obs}); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if idx(obs.cleaned, "retired") < 0 {
		t.Fatalf("cleaned = %v, want %q (run-scoped) released even though the workflow no longer declares it", obs.cleaned, "retired")
	}
	s := store.Get("o/r-1")
	if s.Nodes["retired"] == nil || s.Nodes["retired"].Status != contract.TaskStatusCleaned {
		t.Errorf("retired should be persisted as cleaned: %+v", s.Nodes["retired"])
	}
	if s.Nodes["review"].Status != contract.TaskStatusProduced {
		t.Errorf("session-scoped review should survive down: %+v", s.Nodes["review"])
	}
}

// TestUp_FailedSetupRetainsCleanupContractAcrossARestart proves a
// failed/partial setup's attempt record and its retained cleanup contract
// survive being read back fresh -- state.Store
// holds nothing in Go memory across calls, so a later, independent read is
// exactly what a restarted process's own first read would see -- and a
// later Destroy can still release it using that retained contract alone,
// with no live task definition required (LoadTaskDefinitions never runs a
// cleanup script's own definition through it).
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
	seedSessionWithNodes(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: "o/r-1"}); err == nil {
		t.Fatal("Up: want the flaky setup's failure surfaced, got nil error")
	}

	s := store.Get("o/r-1")
	if s == nil || s.Nodes["flaky"] == nil || s.Nodes["flaky"].Status != contract.TaskStatusFailed {
		t.Fatalf("after Up failure, flaky = %+v, want a retained failed attempt", s.Nodes["flaky"])
	}
	if len(s.Nodes["flaky"].Cleanup) == 0 {
		t.Fatal("after Up failure, flaky has no retained cleanup contract")
	}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "o/r-1"}); err != nil {
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

// TestUnifiedTeardownList_ReleasesNodeRemovedFromCurrentWorkflow proves a
// produced node the current workflow no longer declares must still be
// enumerated for teardown, using its own retained identity rather than the
// current plan (which has nothing to say about a node it doesn't declare
// at all).
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

// TestUnifiedTeardownList_UsesRetainedCleanupWhenCurrentDefinitionIsGone
// proves a removed node's own retained cleanup contract is what teardown
// actually resolves to, not a re-resolution attempt against the (now
// entirely absent) current definition.
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

// TestUnifiedTeardownList_OrdersByRecordedDependencyNotSeqAlone proves
// release ordering follows each execution's own recorded DependsOn edge
// even when it disagrees with plain ascending-Seq order (the case a
// dependent's Seq happens to be lower than its prerequisite's, e.g. after
// the prerequisite's own later generation). RunCleanup reclaims this list
// in reverse, so the dependent ("b") must come out AFTER its prerequisite
// ("a") for "b" to release first.
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
