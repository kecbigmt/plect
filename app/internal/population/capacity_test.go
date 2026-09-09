package population

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/admitstatus"
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
	coordinator := newCapacityCoordinator(func() *config.Config { return &config.Config{} }, store, logStore, admitstatus.NewCache(logStore))
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
			logStore := eventlog.NewStore(store.Dir())
			coordinator := newCapacityCoordinator(func() *config.Config { return cfg }, store, logStore, admitstatus.NewCache(logStore))
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

// Both a broken and a genuinely idle member occupy the cap, so success here
// can only mean the broken one stopped blocking the idle one's eviction.
func TestCapacityPriorityIgnoresMemberWithNonCapacityFailure(t *testing.T) {
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
	limit := 2
	cfg.MaxUpChildren = &limit
	definitions, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	definitions[0].Population.AutoDown = true // required for the idle member below to be evictable
	def := definitions[0]
	store := state.NewStore(t.TempDir())
	logStore := eventlog.NewStore(store.Dir())
	coordinator := newCapacityCoordinator(func() *config.Config { return cfg }, store, logStore, admitstatus.NewCache(logStore))
	coordinator.setDefinitions(definitions)

	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	addCapacityMember(t, def, store, logStore, "idle+agent", "urn:case:idle", base, base.Add(time.Minute))

	if err := store.UpdatePopulation(populationKey(def), func(population *state.PopulationState) error {
		population.Members["urn:case:new"] = &state.PopulationMember{ResourceID: "urn:case:new", PendingUp: true}
		population.Members["urn:case:broken"] = &state.PopulationMember{
			ResourceID: "urn:case:broken", SessionName: "broken+agent", PendingUp: true,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&contract.Session{
		Name: "broken+agent", ResourceID: "urn:case:broken",
		Population: &contract.PopulationProvenance{Workflow: def.Workflow.Address, Name: def.Population.Name},
		Tasks: map[string]*contract.TaskState{
			"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := logStore.Append(event.Event{
		SessionName: "broken+agent",
		Time:        base,
		Type:        event.TypeWorkflowPopulationFailure,
		Direction:   event.Internal,
		Metadata:    map[string]string{"reason": "input", "resource": "urn:case:broken"},
	}); err != nil {
		t.Fatal(err)
	}

	outcome, err := coordinator.up(context.Background(), def, "urn:case:new", map[string]any{"resource": "urn:case:new"})
	if err != nil {
		t.Fatalf("up: %v, want the chronically broken member to lose priority so the idle member is evicted instead", err)
	}
	if outcome.SessionName == "" || store.Get(outcome.SessionName) == nil {
		t.Fatalf("session = %q, want a newly admitted population session", outcome.SessionName)
	}
	if idle := store.Get("idle+agent"); idle == nil || cfg.RunScopeUp(idle) {
		t.Fatalf("idle session = %+v, want it brought down to free capacity", idle)
	}
}

func TestCapacityPriorityRetainedAfterCapacityRefusal(t *testing.T) {
	coordinator, def, store, logStore, base := capacityFixture(t)
	if err := store.UpdatePopulation(populationKey(def), func(population *state.PopulationState) error {
		population.Members["urn:case:new"] = &state.PopulationMember{ResourceID: "urn:case:new", PendingUp: true}
		population.Members["urn:case:queued"] = &state.PopulationMember{
			ResourceID: "urn:case:queued", SessionName: "queued+agent", PendingUp: true,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := logStore.Append(event.Event{
		SessionName: "queued+agent",
		Time:        base,
		Type:        event.TypeWorkflowPopulationFailure,
		Direction:   event.Internal,
		Metadata:    map[string]string{"reason": "capacity", "resource": "urn:case:queued"},
	}); err != nil {
		t.Fatal(err)
	}

	blocker, blocked := coordinator.pendingExistingAhead(def, "urn:case:new")
	if !blocked || blocker.session != "queued+agent" {
		t.Fatalf("blocker = %+v, blocked = %v, want the capacity-refused member to keep priority", blocker, blocked)
	}
}

// TestCapacityPriorityExcludesProvenanceConflict guards a real bug found in
// review: a provenance conflict is its own event type, not a failure, and
// was invisible to the priority classifier — a member stuck in an
// unresolvable naming conflict read as eligible and could hold priority
// forever, exactly like the original starvation bug this PR fixes.
func TestCapacityPriorityExcludesProvenanceConflict(t *testing.T) {
	coordinator, def, store, logStore, base := capacityFixture(t)
	if err := store.UpdatePopulation(populationKey(def), func(population *state.PopulationState) error {
		population.Members["urn:case:new"] = &state.PopulationMember{ResourceID: "urn:case:new", PendingUp: true}
		population.Members["urn:case:conflicted"] = &state.PopulationMember{
			ResourceID: "urn:case:conflicted", SessionName: "conflicted+agent", PendingUp: true,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := logStore.Append(event.Event{
		SessionName: "conflicted+agent",
		Time:        base,
		Type:        event.TypeWorkflowPopulationConflict,
		Direction:   event.Internal,
		Metadata:    map[string]string{"reason": "provenance", "resource": "urn:case:conflicted"},
	}); err != nil {
		t.Fatal(err)
	}

	if blocker, blocked := coordinator.pendingExistingAhead(def, "urn:case:new"); blocked {
		t.Fatalf("blocker = %+v, want a provenance-conflicted member to lose priority", blocker)
	}
}

// TestCapacityPriorityRecoversAfterAdmitOK guards the case
// TestCapacityPriorityIgnoresMemberWithNonCapacityFailure cannot: a member
// that failed once but has since admitted successfully must not stay
// disqualified by that stale failure the next time it goes pending.
func TestCapacityPriorityRecoversAfterAdmitOK(t *testing.T) {
	coordinator, def, store, logStore, base := capacityFixture(t)
	if err := store.UpdatePopulation(populationKey(def), func(population *state.PopulationState) error {
		population.Members["urn:case:new"] = &state.PopulationMember{ResourceID: "urn:case:new", PendingUp: true}
		population.Members["urn:case:recovered"] = &state.PopulationMember{
			ResourceID: "urn:case:recovered", SessionName: "recovered+agent", PendingUp: true,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := logStore.Append(event.Event{
		SessionName: "recovered+agent",
		Time:        base,
		Type:        event.TypeWorkflowPopulationFailure,
		Direction:   event.Internal,
		Metadata:    map[string]string{"reason": "input", "resource": "urn:case:recovered"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := logStore.Append(event.Event{
		SessionName: "recovered+agent",
		Time:        base.Add(time.Minute),
		Type:        event.TypeWorkflowPopulationAdmitOK,
		Direction:   event.Internal,
		Metadata:    map[string]string{"reason": "admit", "resource": "urn:case:recovered"},
	}); err != nil {
		t.Fatal(err)
	}

	blocker, blocked := coordinator.pendingExistingAhead(def, "urn:case:new")
	if !blocked || blocker.session != "recovered+agent" {
		t.Fatalf("blocker = %+v, blocked = %v, want a member that admitted successfully since its last failure to hold priority again", blocker, blocked)
	}
}

// TestCapacityRefusalSurvivesEmptyIdleCandidates guards a separate,
// pre-existing bug found while adding the test above: an empty (non-error)
// idleCandidates result must not clobber the original capacity refusal
// into a false success.
func TestCapacityRefusalSurvivesEmptyIdleCandidates(t *testing.T) {
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
	logStore := eventlog.NewStore(store.Dir())
	coordinator := newCapacityCoordinator(func() *config.Config { return cfg }, store, logStore, admitstatus.NewCache(logStore))
	coordinator.setDefinitions(definitions)

	// Fills the cap with a session that is not a population member, so it
	// can never be an eviction candidate: the only path left is the
	// idleCandidates-empty fallback.
	if err := store.Put(&contract.Session{Name: "manual", Tasks: map[string]*contract.TaskState{
		"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
	}}); err != nil {
		t.Fatal(err)
	}

	outcome, err := coordinator.up(context.Background(), def, "urn:case:new", map[string]any{"resource": "urn:case:new"})
	if err == nil {
		t.Fatalf("up returned no error with outcome %+v, want the capacity refusal preserved", outcome)
	}
	if outcome.SessionName != "" {
		t.Fatalf("outcome = %+v, want no session on a capacity refusal", outcome)
	}
}

func addCapacityMember(t *testing.T, def Definition, store *state.Store, logStore *eventlog.Store, name, resource string, created, cleared time.Time) {
	t.Helper()
	// The population row must exist before a session can reference it
	// (sessions.population_workflow/population_name FK), matching
	// production's own order: ApplyPoll/ApplyAppearance always upserts the
	// population before Reconcile ever admits a session into it.
	if err := store.UpdatePopulation(populationKey(def), func(population *state.PopulationState) error {
		population.Workflow = def.Workflow.Address
		population.Name = def.Population.Name
		population.Members[resource] = &state.PopulationMember{ResourceID: resource, SessionName: name, Generation: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&contract.Session{
		Name:       name,
		ResourceID: resource,
		Workflow:   def.Workflow.Address,
		Population: &contract.PopulationProvenance{Workflow: def.Workflow.Address, Name: def.Population.Name},
		Tasks: map[string]*contract.TaskState{
			"runtime": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
		CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}
	if !cleared.IsZero() {
		if _, _, _, err := logStore.Append(event.Event{
			SessionName: name,
			Time:        cleared,
			Type:        event.TypeStatusMessage,
			Metadata:    map[string]string{"cleared": "true"}, Direction: event.Internal,
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

	candidates, err := coordinator.idleCandidates()
	if err != nil {
		t.Fatalf("idleCandidates: %v", err)
	}
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

	candidates, err := coordinator.idleCandidates()
	if err != nil {
		t.Fatalf("idleCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want inbound activity to invalidate earlier idle evidence", candidates)
	}
}

func TestCapacityCandidatesExcludeAutoDownFalse(t *testing.T) {
	coordinator, def, store, logStore, base := capacityFixture(t)
	def.Population.AutoDown = false
	coordinator.setDefinitions([]Definition{def})
	addCapacityMember(t, def, store, logStore, "member", "urn:case:member", base, base.Add(time.Minute))

	candidates, err := coordinator.idleCandidates()
	if err != nil {
		t.Fatalf("idleCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %+v, want auto_down=false excluded", candidates)
	}
}
