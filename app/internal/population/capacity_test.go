package population

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func capacityFixture(t *testing.T) (*capacityCoordinator, Definition, *state.Store, *eventlog.Store, time.Time) {
	t.Helper()
	store := state.NewStore(t.TempDir())
	logStore := eventlog.NewStore(store.Dir())
	def := Definition{
		Workflow:   config.WorkflowFile{Address: "agent"},
		Population: config.WorkflowPopulation{Name: "dispatch", AutoDown: true},
	}
	coordinator := newCapacityCoordinator(func() *config.Config { return &config.Config{} }, store, logStore)
	coordinator.setDefinitions([]Definition{def})
	return coordinator, def, store, logStore, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
}

func TestCapacityPriorityOnlyAppliesWhenVirtualRootCapIsFull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seedFull bool
		wantErr  bool
	}{
		{name: "under cap"},
		{name: "at cap", seedFull: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			limit := 1
			cfg.MaxUpChildren = &limit
			definitions, err := Load(cfg)
			if err != nil {
				t.Fatal(err)
			}
			def := definitions[0]
			store := state.NewStore(t.TempDir())
			coordinator := newCapacityCoordinator(func() *config.Config { return cfg }, store, eventlog.NewStore(store.Dir()))
			coordinator.setDefinitions(definitions)
			if err := store.UpdatePopulation(populationKey(def), func(population *state.PopulationState) error {
				population.Members["urn:case:new"] = &state.PopulationMember{ResourceID: "urn:case:new", PendingUp: true}
				population.Members["urn:case:owned"] = &state.PopulationMember{
					ResourceID: "urn:case:owned", SessionName: "owned+agent", PendingUp: true,
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Put(&contract.Session{
				Name: "owned+agent", ResourceID: "urn:case:owned",
				Population: &contract.PopulationProvenance{Workflow: def.Workflow.Address, Name: def.Population.Name},
			}); err != nil {
				t.Fatal(err)
			}
			if tc.seedFull {
				if err := store.Put(&contract.Session{Name: "manual", Tasks: map[string]*contract.TaskState{
					"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
				}}); err != nil {
					t.Fatal(err)
				}
			}

			outcome, err := coordinator.up(context.Background(), def, "urn:case:new", map[string]any{"resource": "urn:case:new"})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "takes priority") {
					t.Fatalf("up error = %v, want existing-member priority after a cap rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("up under the virtual-root cap: %v", err)
			}
			if outcome.SessionName == "" || store.Get(outcome.SessionName) == nil {
				t.Fatalf("session = %q, want a newly admitted population session", outcome.SessionName)
			}
			if outcome.AlreadyUp {
				t.Fatal("a newly admitted session reported as already up")
			}
		})
	}
}

func addCapacityMember(t *testing.T, def Definition, store *state.Store, logStore *eventlog.Store, name, resource string, created, cleared time.Time) {
	t.Helper()
	if err := store.Put(&contract.Session{
		Name:       name,
		ResourceID: resource,
		Population: &contract.PopulationProvenance{Workflow: def.Workflow.Address, Name: def.Population.Name},
		Tasks: map[string]*contract.TaskState{
			"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePopulation(populationKey(def), func(population *state.PopulationState) error {
		population.Workflow = def.Workflow.Address
		population.Name = def.Population.Name
		population.Members[resource] = &state.PopulationMember{ResourceID: resource, SessionName: name, Generation: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !cleared.IsZero() {
		if _, _, _, err := logStore.Append(event.Event{
			SessionName: name,
			Time:        cleared,
			Type:        event.TypeStatusMessage,
			Metadata:    map[string]string{"cleared": "true"},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCapacityCandidatesRequireExplicitIdleAndUseOldestActivity(t *testing.T) {
	coordinator, def, store, logStore, base := capacityFixture(t)
	addCapacityMember(t, def, store, logStore, "newer", "urn:case:newer", base, base.Add(3*time.Minute))
	addCapacityMember(t, def, store, logStore, "older", "urn:case:older", base, base.Add(time.Minute))
	addCapacityMember(t, def, store, logStore, "never-reported", "urn:case:never", base, time.Time{})

	candidates := coordinator.idleCandidates()
	if len(candidates) != 2 || candidates[0].session != "older" || candidates[1].session != "newer" {
		t.Fatalf("candidates = %+v, want explicitly idle members oldest first", candidates)
	}
}

func TestCapacityCandidateIdleEvidenceIsInvalidatedByInboundEvent(t *testing.T) {
	coordinator, def, store, logStore, base := capacityFixture(t)
	addCapacityMember(t, def, store, logStore, "member", "urn:case:member", base, base.Add(time.Minute))
	if _, _, _, err := logStore.Append(event.Event{
		SessionName: "member",
		Time:        base.Add(2 * time.Minute),
		Type:        "external.message",
		Direction:   event.Inbound,
	}); err != nil {
		t.Fatal(err)
	}

	if candidates := coordinator.idleCandidates(); len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want inbound activity to invalidate earlier idle evidence", candidates)
	}
}

func TestCapacityCandidatesExcludeAutoDownFalse(t *testing.T) {
	coordinator, def, store, logStore, base := capacityFixture(t)
	def.Population.AutoDown = false
	coordinator.setDefinitions([]Definition{def})
	addCapacityMember(t, def, store, logStore, "member", "urn:case:member", base, base.Add(time.Minute))

	if candidates := coordinator.idleCandidates(); len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want auto_down=false excluded", candidates)
	}
}
