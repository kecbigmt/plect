package population

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func TestFailedInitialTaskIsNotAcceptedAsSuccessfullyInstalled(t *testing.T) {
	store := state.NewStore(t.TempDir())
	if err := store.Put(&contract.Session{
		Name: "member",
		Tasks: map[string]*contract.TaskState{
			"initial": {
				Name: "initial", TaskID: "work", Resource: "urn:case:a",
				Dynamic: true, Status: contract.TaskStatusFailed,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	hooks := serviceHooks(func() *config.Config { return nil }, store, Definition{}, nil)
	if err := hooks.EnsureInitial(context.Background(), "member", "work", "urn:case:a"); err == nil {
		t.Fatal("failed initial task was accepted as installed")
	}
}

func TestPopulationCannotAdoptSameProvenanceSessionForAnotherResource(t *testing.T) {
	cfg := populationConfig(t, `[source.query.poll]
type = "exec"
command = "true"
`, "uses = [\"poll\"]\npoll_every = \"1m\"", `resource = { from = "resource.id" }`)
	definitions, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	definition := definitions[0]
	store := state.NewStore(t.TempDir())
	provenance := &contract.PopulationProvenance{Workflow: definition.Workflow.Address, Name: definition.Population.Name}
	name, err := service.ResolvePopulationSessionName(cfg, definition.Workflow.Address, "urn:case:a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&contract.Session{Name: name, ResourceID: "urn:case:b", Workflow: definition.Workflow.Address, Population: provenance}); err != nil {
		t.Fatal(err)
	}

	_, err = upPopulation(func() *config.Config { return cfg }, store, definition, provenance, "urn:case:a", nil)
	var conflict *populationConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want population identity conflict", err)
	}
}

func TestUpPopulationReportsRunStateFromBeforeTheIdempotentUpCall(t *testing.T) {
	// The lifecycle guards read the ambient session from the environment, so
	// an inherited one would decide this test's parenting and its down step.
	t.Setenv("PLECT_SESSION_NAME", "")
	cfg := populationConfig(t, `[source.query.poll]
type = "exec"
command = "true"
`, "uses = [\"poll\"]\npoll_every = \"1m\"", `resource = { from = "resource.id" }`)
	writeDefinition(t, cfg.BaseDir, "provider", fmt.Sprintf(`[provider]
kind = "workspace_provider"
match = "^urn:case:(?P<id>[A-Za-z0-9]+)$"
name = { from = "match.id" }
[provider.setup]
type = "exec"
command = "printf"
args = ['{"workspace_dir":"%s","branch":"main"}']
[provider.outputs_schema]
type = "object"
`, cfg.WorkspaceDirsRoot))
	definitions, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	definition := definitions[0]
	store := state.NewStore(t.TempDir())
	provenance := &contract.PopulationProvenance{Workflow: definition.Workflow.Address, Name: definition.Population.Name}
	up := func() UpOutcome {
		t.Helper()
		outcome, upErr := upPopulation(func() *config.Config { return cfg }, store, definition, provenance,
			"urn:case:a", map[string]any{"resource": "urn:case:a"})
		if upErr != nil {
			t.Fatalf("upPopulation: %v", upErr)
		}
		return outcome
	}

	first := up()
	if first.SessionName == "" || first.AlreadyUp {
		t.Fatalf("first admission = %+v, want a new session reported as a transition", first)
	}
	if second := up(); !second.AlreadyUp {
		t.Fatalf("re-admission of a member that never went down = %+v, want no transition", second)
	}
	if _, err := service.Down(cfg, store, service.DownParams{Identifier: first.SessionName}); err != nil {
		t.Fatal(err)
	}
	if resumed := up(); resumed.AlreadyUp {
		t.Fatalf("resuming a down member = %+v, want a transition", resumed)
	}
}
