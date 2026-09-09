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

// TestMemberCountsProvenanceConflict guards a real bug found in review: a
// provenance conflict (plect.workflow_population.conflict) is not a
// plect.workflow_population.failure at all, so it was invisible to the
// classifier — a member stuck in an unresolvable naming conflict read as
// eligible and could hold priority forever.
func TestMemberCountsProvenanceConflict(t *testing.T) {
	log := eventlog.NewStore(state.NewStore(t.TempDir()).Dir())
	if _, _, _, err := log.Append(event.Event{
		SessionName: "a+agent",
		Type:        event.TypeWorkflowPopulationConflict,
		Direction:   event.Internal,
		Summary:     "owned elsewhere",
		Metadata:    map[string]string{"reason": "provenance", "resource": "urn:case:a"},
	}); err != nil {
		t.Fatal(err)
	}

	status := Member(log, "a+agent", "urn:case:a")
	if status.LastReason != "provenance" || status.LastError != "owned elsewhere" || status.Consecutive != 1 {
		t.Fatalf("status = %+v, want the conflict counted as a disqualifying outcome", status)
	}
}

// TestMemberFindsAdmitFailureBehindManyUnrelatedEvents guards a real bug
// found in review in an earlier, bounded-lookback version of this scan: a
// window narrow enough to bound cost, applied before resource/reason
// filtering (event.Filter has no resource-scoped variant), could silently
// drop the one admit failure that mattered behind a run of unrelated
// poll/subscribe noise — recreating the starvation bug this package exists
// to prevent. Member must find it regardless of how much noise follows.
func TestMemberFindsAdmitFailureBehindManyUnrelatedEvents(t *testing.T) {
	store := state.NewStore(t.TempDir())
	log := eventlog.NewStore(store.Dir())
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	if _, _, _, err := log.Append(event.Event{
		SessionName: "a+agent", Time: base,
		Type: event.TypeWorkflowPopulationFailure, Direction: event.Internal,
		Metadata: map[string]string{"reason": "input", "resource": "urn:case:a"},
	}); err != nil {
		t.Fatal(err)
	}
	const noise = 200
	for i := 1; i <= noise; i++ {
		if _, _, _, err := log.Append(event.Event{
			SessionName: "a+agent", Time: base.Add(time.Duration(i) * time.Minute),
			Type: event.TypeWorkflowPopulationFailure, Direction: event.Internal,
			Metadata: map[string]string{"reason": "poll", "resource": "urn:case:a"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	status := Member(log, "a+agent", "urn:case:a")
	if status.LastReason != "input" {
		t.Fatalf("status = %+v, want the admit failure behind %d unrelated events still found", status, noise)
	}
}

// TestCacheGetSeedsOnceThenIgnoresExternalWrites proves the contract the
// capacity gate depends on: Get scans the log at most once per member (the
// hot path must never scan on every pass), and only Record — not a write
// straight to the log — changes what a later Get returns.
func TestCacheGetSeedsOnceThenIgnoresExternalWrites(t *testing.T) {
	log := eventlog.NewStore(state.NewStore(t.TempDir()).Dir())
	cache := NewCache(log)

	if got := cache.Get("a+agent", "urn:case:a").LastReason; got != "" {
		t.Fatalf("initial status = %q, want eligible with no history", got)
	}

	if _, _, _, err := log.Append(event.Event{
		SessionName: "a+agent", Type: event.TypeWorkflowPopulationFailure, Direction: event.Internal,
		Metadata: map[string]string{"reason": "input", "resource": "urn:case:a"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := cache.Get("a+agent", "urn:case:a").LastReason; got != "" {
		t.Fatalf("status after a log write bypassing Record = %q, want the cached value unchanged", got)
	}

	cache.Record("a+agent", "urn:case:a", false, "input", "boom")
	if got := cache.Get("a+agent", "urn:case:a").LastReason; got != "input" {
		t.Fatalf("status after Record = %q, want input", got)
	}
}

func TestCacheRecordSuccessResetsStreak(t *testing.T) {
	cache := NewCache(eventlog.NewStore(state.NewStore(t.TempDir()).Dir()))
	cache.Record("a+agent", "urn:case:a", false, "up", "boom")
	cache.Record("a+agent", "urn:case:a", false, "up", "boom")
	if got := cache.Get("a+agent", "urn:case:a").Consecutive; got != 2 {
		t.Fatalf("consecutive = %d, want 2 before the reset", got)
	}

	cache.Record("a+agent", "urn:case:a", true, "", "")
	status := cache.Get("a+agent", "urn:case:a")
	if status.LastReason != "" || status.Consecutive != 0 {
		t.Fatalf("status after a successful admit = %+v, want a full reset", status)
	}
}

func TestCacheGetIsolatesResourcesOnTheSameSession(t *testing.T) {
	cache := NewCache(eventlog.NewStore(state.NewStore(t.TempDir()).Dir()))
	cache.Record("shared+agent", "urn:case:a", false, "input", "boom")
	if got := cache.Get("shared+agent", "urn:case:b").LastReason; got != "" {
		t.Fatalf("status for a different resource on the same session = %q, want unaffected", got)
	}
}
