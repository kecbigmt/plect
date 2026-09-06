package service

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// nodeResultEvents reads every plect.node.result event recorded for a
// session, in append order.
func nodeResultEvents(t *testing.T, store interface{ Dir() string }, sessionName string) []event.Event {
	t.Helper()
	evs, _, _, err := eventlog.NewStore(store.Dir()).List(sessionName, 0, event.Filter{Types: []string{event.TypeNodeResult}})
	if err != nil {
		t.Fatalf("list node.result events: %v", err)
	}
	return evs
}

func TestUp_RecordsNodeResultForProducedNode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "agent", scope: "run", setup: `echo '{}'`}},
		[]nodeFixture{{id: "agent"}},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	evs := nodeResultEvents(t, store, sessionName)
	if len(evs) != 1 {
		t.Fatalf("node.result events = %d, want 1: %+v", len(evs), evs)
	}
	got := evs[0]
	if got.Type != event.TypeNodeResult || got.Source != event.SourcePlect || got.Direction != event.Internal {
		t.Fatalf("event = %+v", got)
	}
	if got.Metadata["node"] != "agent" || got.Metadata["action"] != event.NodeResultActionSetup || got.Metadata["result"] != event.NodeResultProduced {
		t.Fatalf("metadata = %+v", got.Metadata)
	}
	if got.Metadata["duration_ms"] == "" {
		t.Fatalf("metadata missing duration_ms: %+v", got.Metadata)
	}
}

func TestUp_RecordsNodeResultForFailedSetup(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "agent", scope: "run", setup: `echo boom 1>&2; exit 1`}},
		[]nodeFixture{{id: "agent"}},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{})

	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err == nil {
		t.Fatal("expected Up to fail")
	}

	evs := nodeResultEvents(t, store, sessionName)
	if len(evs) != 1 {
		t.Fatalf("node.result events = %d, want 1: %+v", len(evs), evs)
	}
	got := evs[0]
	if got.Metadata["action"] != event.NodeResultActionSetup || got.Metadata["result"] != event.NodeResultFailed {
		t.Fatalf("metadata = %+v", got.Metadata)
	}
	if got.Body == "" {
		t.Fatalf("expected a bounded stderr tail in body, got empty")
	}
}

func TestUp_RecordsNodeResultForAliveSkip(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "agent", scope: "run", setup: `echo '{}'`, alive: "true"}},
		[]nodeFixture{{id: "agent"}},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{
		"agent": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	evs := nodeResultEvents(t, store, sessionName)
	if len(evs) != 1 {
		t.Fatalf("node.result events = %d, want 1: %+v", len(evs), evs)
	}
	if got := evs[0].Metadata; got["action"] != event.NodeResultActionAlive || got["result"] != event.NodeResultSkipped {
		t.Fatalf("metadata = %+v", got)
	}
}

func TestDown_RecordsNodeResultForCleanedNode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "agent", scope: "run", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "agent"}},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{
		"agent": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	if _, err := Down(cfg, store, DownParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Down: %v", err)
	}

	evs := nodeResultEvents(t, store, sessionName)
	if len(evs) != 1 {
		t.Fatalf("node.result events = %d, want 1: %+v", len(evs), evs)
	}
	if got := evs[0].Metadata; got["action"] != event.NodeResultActionCleanup || got["result"] != event.NodeResultCleaned {
		t.Fatalf("metadata = %+v", got)
	}
}

// The member never transitions from down to up during an in-place repair —
// it was already up throughout — so this asserts plect.node.result lands on
// its own, without a plect.workflow_population.up alongside it.
func TestUp_PopulationMemberRepairRecordsNodeResultWithoutUpTransition(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "session1"
	pane := filepath.Join(t.TempDir(), "pane")
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{id: "pane", scope: "run", setup: "touch " + pane + "; echo '{}'", alive: "test -f " + pane},
		},
		[]nodeFixture{{id: "pane"}},
	)
	seedSession(t, store, sessionName, "acct", 1, "default", map[string]*contract.TaskState{
		"pane": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.Population = &contract.PopulationProvenance{Workflow: "default", Name: "pop"}
		return nil
	}); err != nil {
		t.Fatalf("set population provenance: %v", err)
	}

	// No Observer: this mirrors the population evaluator's own repair call
	// (internal/population/runtime.go), which passes none.
	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// The vanished pane fails its liveness check, so the repair is three
	// separate node.result facts: the failed check itself, the cleanup it
	// triggers, and the re-setup that follows — not one.
	evs := nodeResultEvents(t, store, sessionName)
	if len(evs) != 3 {
		t.Fatalf("node.result events = %d, want 3: %+v", len(evs), evs)
	}
	if got := evs[0].Metadata; got["node"] != "pane" || got["action"] != event.NodeResultActionAlive || got["result"] != event.NodeResultFailed {
		t.Fatalf("first event metadata = %+v", got)
	}
	if got := evs[1].Metadata; got["node"] != "pane" || got["action"] != event.NodeResultActionCleanup || got["result"] != event.NodeResultCleaned {
		t.Fatalf("second event metadata = %+v", got)
	}
	if got := evs[2].Metadata; got["node"] != "pane" || got["action"] != event.NodeResultActionSetup || got["result"] != event.NodeResultProduced {
		t.Fatalf("third event metadata = %+v", got)
	}

	upEvents, _, _, err := eventlog.NewStore(store.Dir()).List(sessionName, 0, event.Filter{Types: []string{event.TypeWorkflowPopulationUp}})
	if err != nil {
		t.Fatalf("list workflow_population.up events: %v", err)
	}
	if len(upEvents) != 0 {
		t.Fatalf("expected no plect.workflow_population.up for an in-place repair, got %+v", upEvents)
	}
}
