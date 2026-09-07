package service

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// seedPopulationRowForTest upserts an empty population row so a test can
// Up a session referencing it directly (sessions.population_workflow/name
// FK), matching production's own order where the engine always creates the
// population before admitting a session into it.
func seedPopulationRowForTest(t *testing.T, store *state.Store, p *contract.PopulationProvenance) {
	t.Helper()
	if err := store.UpdatePopulation(p.Workflow+"/"+p.Name, func(population *state.PopulationState) error {
		population.Workflow = p.Workflow
		population.Name = p.Name
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpPopulationProvenanceCannotAdoptAnExistingSession(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cfg := writeWorkflowFixture(t, filepath.Join(t.TempDir(), "workdirs"), "agent",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "agent", capProviderCreatingWorkspace("agent", workdir))
	resource := "https://example.test/cases/owned"
	owner := &contract.PopulationProvenance{Workflow: "agent", Name: "first"}
	seedPopulationRowForTest(t, store, owner)
	result, err := Up(cfg, store, UpParams{Identifier: resource, Workflow: "agent", Population: owner})
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if got := store.Get(result.SessionName).Population; got == nil || *got != *owner {
		t.Fatalf("population = %+v, want %+v", got, owner)
	}

	other := &contract.PopulationProvenance{Workflow: "agent", Name: "second"}
	if _, err := Up(cfg, store, UpParams{Identifier: resource, Workflow: "agent", Population: other}); err == nil {
		t.Fatal("a different population adopted the existing session")
	}
	if got := store.Get(result.SessionName).Population; got == nil || *got != *owner {
		t.Fatalf("population changed after collision: %+v", got)
	}
}

func TestChainStyleDispatchCannotAdoptAPopulationSession(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cfg := writeWorkflowFixture(t, filepath.Join(t.TempDir(), "workdirs"), "agent",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "agent", capProviderCreatingWorkspace("agent", workdir))
	resource := "https://example.test/cases/owned"
	owner := &contract.PopulationProvenance{Workflow: "agent", Name: "dispatch"}
	seedPopulationRowForTest(t, store, owner)
	if _, err := Up(cfg, store, UpParams{Identifier: resource, Workflow: "agent", Population: owner}); err != nil {
		t.Fatalf("population Up: %v", err)
	}

	_, err := Up(cfg, store, UpParams{Identifier: resource, Workflow: "agent", ParentSession: "work-session"})
	if err == nil || !strings.Contains(err.Error(), "population") {
		t.Fatalf("chain-style Up error = %v, want population ownership conflict", err)
	}
}

func TestPopulationTaskBlockersFailsClosedAndClearsOnlySatisfiedTasks(t *testing.T) {
	missingPolicy := `[unbounded]
kind              = "task"
resource_observer = "issue_pr"
instructions      = [{ text = "Handle {{ resource.id }}." }]
`
	store := testStore(t)
	cfg := writeTaskDocumentFixture(t, t.TempDir(), "wf",
		map[string]string{"resource_kind": "pull", "revision": "sha2"}, reviewDocument, missingPolicy)
	seedSession(t, store, "session-a", "workspace-a", 1, "wf", map[string]*contract.TaskState{
		"review#1": {
			Scope: contract.TaskScopeSession, TaskID: "review", Status: contract.TaskStatusProduced,
			Resource: "https://example.test/pull/1", State: map[string]any{"verdict_revision": "sha1"}, SetupAt: time.Now(),
		},
		"unbounded#1": {
			Scope: contract.TaskScopeSession, TaskID: "unbounded", Status: contract.TaskStatusProduced,
			Resource: "https://example.test/pull/2", SetupAt: time.Now(),
		},
		"failed#1": {
			Scope: contract.TaskScopeSession, TaskID: "review", Status: contract.TaskStatusFailed,
			Resource: "https://example.test/pull/3", SetupAt: time.Now(),
		},
	})

	blockers, err := PopulationTaskBlockers(cfg, store, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(blockers, []string{"failed#1", "review#1", "unbounded#1"}) {
		t.Fatalf("blockers = %v, want pending predicate and missing policy", blockers)
	}
	if err := store.Update("session-a", func(session *domain.Session) error {
		session.Tasks["review#1"].State["verdict_revision"] = "sha2"
		session.Tasks["unbounded#1"].Status = contract.TaskStatusCleaned
		session.Tasks["failed#1"].Status = contract.TaskStatusCleaned
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blockers, err = PopulationTaskBlockers(cfg, store, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %v, want all produced dynamic tasks satisfied or gone", blockers)
	}
}
