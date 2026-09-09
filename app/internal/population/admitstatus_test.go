package population

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

func TestMemberAdmitStatusNoHistoryIsEligible(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	member := &state.PopulationMember{ResourceID: "urn:case:a", SessionName: "a+agent"}

	status := memberAdmitStatus(log, member)
	if status.LastReason != "" || status.Consecutive != 0 {
		t.Fatalf("status = %+v, want a member with no recorded outcome to read as eligible", status)
	}
}

func TestMemberAdmitStatusCountsConsecutiveFailuresSinceLastAdmitOK(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	member := &state.PopulationMember{ResourceID: "urn:case:a", SessionName: "a+agent"}
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)

	for i, ev := range []event.Event{
		{Type: event.TypeWorkflowPopulationFailure, Metadata: map[string]string{"reason": "input", "resource": "urn:case:a"}},
		{Type: event.TypeWorkflowPopulationAdmitOK, Metadata: map[string]string{"reason": "admit", "resource": "urn:case:a"}},
		{Type: event.TypeWorkflowPopulationFailure, Metadata: map[string]string{"reason": "up", "resource": "urn:case:a"}},
		{Type: event.TypeWorkflowPopulationFailure, Metadata: map[string]string{"reason": "task_setup", "resource": "urn:case:a"}},
	} {
		ev.SessionName = "a+agent"
		ev.Time = base.Add(time.Duration(i) * time.Minute)
		ev.Direction = event.Internal
		if _, _, _, err := log.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	status := memberAdmitStatus(log, member)
	if status.LastReason != "task_setup" || status.Consecutive != 2 {
		t.Fatalf("status = %+v, want the two failures since the last admit_ok, latest reason first", status)
	}
}

func TestMemberAdmitStatusIgnoresAnotherResourceOnTheSameSession(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	member := &state.PopulationMember{ResourceID: "urn:case:a", SessionName: "shared+agent"}
	if _, _, _, err := log.Append(event.Event{
		SessionName: "shared+agent",
		Type:        event.TypeWorkflowPopulationFailure,
		Direction:   event.Internal,
		Metadata:    map[string]string{"reason": "input", "resource": "urn:case:other"},
	}); err != nil {
		t.Fatal(err)
	}

	status := memberAdmitStatus(log, member)
	if status.LastReason != "" {
		t.Fatalf("status = %+v, want a failure recorded against a different resource to be ignored", status)
	}
}

func TestMemberAdmitStatusEmptySessionIsEligible(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	member := &state.PopulationMember{ResourceID: "urn:case:a"}

	status := memberAdmitStatus(log, member)
	if status.LastReason != "" || status.Consecutive != 0 {
		t.Fatalf("status = %+v, want a member with no session yet to read as eligible", status)
	}
}
