package admitstatus

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

func TestMemberNoHistoryIsEligible(t *testing.T) {
	log := eventlog.NewStore(state.NewStore(t.TempDir()).Dir())
	status := Member(log, "a+agent", "urn:case:a")
	if status.LastReason != "" || status.Consecutive != 0 {
		t.Fatalf("status = %+v, want no recorded outcome to read as eligible", status)
	}
}

func TestMemberEmptySessionIsEligible(t *testing.T) {
	log := eventlog.NewStore(state.NewStore(t.TempDir()).Dir())
	status := Member(log, "", "urn:case:a")
	if status.LastReason != "" || status.Consecutive != 0 {
		t.Fatalf("status = %+v, want a member with no session yet to read as eligible", status)
	}
}

func TestMemberCountsConsecutiveFailuresSinceLastAdmitOK(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
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

	status := Member(log, "a+agent", "urn:case:a")
	if status.LastReason != "task_setup" || status.Consecutive != 2 {
		t.Fatalf("status = %+v, want the two failures since the last admit_ok, latest reason first", status)
	}
}

func TestMemberIgnoresAnotherResourceOnTheSameSession(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	if _, _, _, err := log.Append(event.Event{
		SessionName: "shared+agent",
		Type:        event.TypeWorkflowPopulationFailure,
		Direction:   event.Internal,
		Metadata:    map[string]string{"reason": "input", "resource": "urn:case:other"},
	}); err != nil {
		t.Fatal(err)
	}

	status := Member(log, "shared+agent", "urn:case:a")
	if status.LastReason != "" {
		t.Fatalf("status = %+v, want a failure recorded against a different resource to be ignored", status)
	}
}

// TestMemberIgnoresNonAdmitFailureReasons guards a real bug found in
// review: plect.workflow_population.failure also records poll, subscribe,
// down (eviction), and destroy failures against a member's own resource —
// none of those are the member's own admit attempt failing, and must not
// disqualify it from capacity-gate priority.
func TestMemberIgnoresNonAdmitFailureReasons(t *testing.T) {
	for _, reason := range []string{"poll", "poll_validation", "subscribe", "subscribe_item", "down", "destroy"} {
		t.Run(reason, func(t *testing.T) {
			log := eventlog.NewStore(state.NewStore(t.TempDir()).Dir())
			if _, _, _, err := log.Append(event.Event{
				SessionName: "a+agent",
				Type:        event.TypeWorkflowPopulationFailure,
				Direction:   event.Internal,
				Metadata:    map[string]string{"reason": reason, "resource": "urn:case:a"},
			}); err != nil {
				t.Fatal(err)
			}
			status := Member(log, "a+agent", "urn:case:a")
			if status.LastReason != "" || status.Consecutive != 0 {
				t.Fatalf("status = %+v, want a %q failure to be ignored as not an admit outcome", status, reason)
			}
		})
	}
}

// TestMemberConsecutiveCountIsExactBeyondAnySmallCap guards against
// reintroducing an artificial cap on the streak count: a real chronic
// failure can run well past a small round number, and the count must stay
// exact rather than plateau.
func TestMemberConsecutiveCountIsExactBeyondAnySmallCap(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	const failures = 120
	for i := 0; i < failures; i++ {
		if _, _, _, err := log.Append(event.Event{
			SessionName: "a+agent",
			Time:        base.Add(time.Duration(i) * time.Minute),
			Type:        event.TypeWorkflowPopulationFailure,
			Direction:   event.Internal,
			Metadata:    map[string]string{"reason": "up", "resource": "urn:case:a"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	status := Member(log, "a+agent", "urn:case:a")
	if status.Consecutive != failures {
		t.Fatalf("consecutive = %d, want the exact count %d", status.Consecutive, failures)
	}
}

func TestMemberAdmitOKResetsAcrossIgnoredFailures(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	for i, ev := range []event.Event{
		{Type: event.TypeWorkflowPopulationFailure, Metadata: map[string]string{"reason": "input", "resource": "urn:case:a"}},
		{Type: event.TypeWorkflowPopulationAdmitOK, Metadata: map[string]string{"reason": "admit", "resource": "urn:case:a"}},
		{Type: event.TypeWorkflowPopulationFailure, Metadata: map[string]string{"reason": "poll", "resource": "urn:case:a"}},
	} {
		ev.SessionName = "a+agent"
		ev.Time = base.Add(time.Duration(i) * time.Minute)
		ev.Direction = event.Internal
		if _, _, _, err := log.Append(ev); err != nil {
			t.Fatal(err)
		}
	}

	status := Member(log, "a+agent", "urn:case:a")
	if status.LastReason != "" || status.Consecutive != 0 {
		t.Fatalf("status = %+v, want a trailing poll failure to leave the post-admit_ok recovery intact", status)
	}
}
