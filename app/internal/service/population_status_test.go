package service

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

func TestPopulationStatusNoSuchPopulationReturnsEmpty(t *testing.T) {
	store := state.NewStore(t.TempDir())
	members, err := PopulationStatus(store, "agent", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if members != nil {
		t.Fatalf("members = %+v, want nil for an unrecorded population", members)
	}
}

func TestPopulationStatusReportsPendingAndLastAdmitFailure(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	if err := store.UpdatePopulation("agent/dispatch", func(population *state.PopulationState) error {
		population.Members["urn:case:a"] = &state.PopulationMember{
			ResourceID: "urn:case:a", SessionName: "a+agent", PendingUp: true,
		}
		population.Members["urn:case:b"] = &state.PopulationMember{ResourceID: "urn:case:b", PendingUp: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := log.Append(event.Event{
		SessionName: "a+agent",
		Type:        event.TypeWorkflowPopulationFailure,
		Direction:   event.Internal,
		Summary:     `"item.thread_ts" resolved to nothing`,
		Metadata:    map[string]string{"reason": "input", "resource": "urn:case:a"},
	}); err != nil {
		t.Fatal(err)
	}

	members, err := PopulationStatus(store, "agent", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members[0].Resource != "urn:case:a" || members[1].Resource != "urn:case:b" {
		t.Fatalf("members = %+v, want both resources sorted", members)
	}
	a := members[0]
	if a.Session != "a+agent" || !a.PendingUp || a.LastAdmitReason != "input" ||
		a.LastAdmitError != `"item.thread_ts" resolved to nothing` || a.ConsecutiveAdmitFailures != 1 {
		t.Fatalf("a = %+v, want its recorded input failure surfaced", a)
	}
	b := members[1]
	if b.Session != "" || b.LastAdmitReason != "" || b.ConsecutiveAdmitFailures != 0 {
		t.Fatalf("b = %+v, want a session-less member to carry no admit history", b)
	}
}

func TestPopulationStatusClearsAfterAdmitOK(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	if err := store.UpdatePopulation("agent/dispatch", func(population *state.PopulationState) error {
		population.Members["urn:case:a"] = &state.PopulationMember{ResourceID: "urn:case:a", SessionName: "a+agent"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []event.Event{
		{Type: event.TypeWorkflowPopulationFailure, Metadata: map[string]string{"reason": "up", "resource": "urn:case:a"}},
		{Type: event.TypeWorkflowPopulationAdmitOK, Metadata: map[string]string{"reason": "admit", "resource": "urn:case:a"}},
	} {
		ev.SessionName = "a+agent"
		ev.Direction = event.Internal
		if _, _, _, err := log.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	members, err := PopulationStatus(store, "agent", "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].LastAdmitReason != "" || members[0].ConsecutiveAdmitFailures != 0 {
		t.Fatalf("members = %+v, want a recovered member to carry no failure", members)
	}
}
