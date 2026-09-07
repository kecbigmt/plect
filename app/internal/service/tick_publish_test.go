package service

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// seedPendingJudgeWork seeds a work session whose done_when has a green check
// leaf and one never-recorded judge leaf, i.e. the pending state a tick turns
// into review_required.
func seedPendingJudgeWork(t *testing.T, store *state.Store, name string) {
	t.Helper()
	seedSession(t, store, name, "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "work",
			Status:   contract.TaskStatusProduced,
			Resource: "https://github.com/owner/repo/pull/1",
			Observed: observedFacts(map[string]any{"checks_status": "SUCCESS", "revision": "sha1"}),
		},
	})
}

func tickEventCount(t *testing.T, store *state.Store, session, eventType string) int {
	t.Helper()
	evs, _, _, err := eventlog.NewStore(store.Dir()).List(session, 0, event.Filter{Types: []string{eventType}})
	if err != nil {
		t.Fatalf("list %s events: %v", eventType, err)
	}
	return len(evs)
}

func setCheckStatus(t *testing.T, store *state.Store, session, status string) {
	t.Helper()
	if err := store.Update(session, func(s *domain.Session) error {
		s.Tasks["initial"].Observed.State["checks_status"] = status
		return nil
	}); err != nil {
		t.Fatalf("update checks_status: %v", err)
	}
}

// Event-triggered ticks are the unbounded ones — the reactor drains its log
// as fast as events arrive — so a done_when state that has not moved must not
// produce a second review_required, no matter how many times it is
// re-evaluated.
func TestTickSession_EventTriggeredRepeatDoesNotRepublishReviewRequired(t *testing.T) {
	store := testStore(t)
	cfg := checkScenarioConfig(t, 4)
	seedPendingJudgeWork(t, store, "owner/repo-1")

	for i := range 4 {
		result, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", Trigger: TickTriggerEvent})
		if err != nil {
			t.Fatalf("TickSession(%d): %v", i, err)
		}
		if len(result.Actions) != 1 || result.Actions[0].Action != "review_required" {
			t.Fatalf("tick %d actions = %+v, want review_required", i, result.Actions)
		}
	}

	if got := tickEventCount(t, store, "owner/repo-1", event.TypeTickReviewRequired); got != 1 {
		t.Fatalf("review_required events = %d, want 1 for four evaluations of one unchanged state", got)
	}
}

// The debounce is per state, not a permanent mute: once the observed facts
// move, the very next event-triggered tick publishes again.
func TestTickSession_EventTriggeredPublishesAgainAfterStateChange(t *testing.T) {
	store := testStore(t)
	cfg := checkScenarioConfig(t, 4)
	seedPendingJudgeWork(t, store, "owner/repo-1")

	if _, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", Trigger: TickTriggerEvent}); err != nil {
		t.Fatalf("first TickSession: %v", err)
	}
	setCheckStatus(t, store, "owner/repo-1", "FAILURE")
	result, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", Trigger: TickTriggerEvent})
	if err != nil {
		t.Fatalf("second TickSession: %v", err)
	}
	if len(result.Actions) != 1 || result.Actions[0].Action != "kick" {
		t.Fatalf("actions after state change = %+v, want kick", result.Actions)
	}
	if got := tickEventCount(t, store, "owner/repo-1", event.TypeUserEmit); got != 1 {
		t.Fatalf("kick events = %d, want 1 after the state changed", got)
	}
}

// A kick carries the same tight-loop risk as review_required — it is
// published into the session's own log on every tick and delivered to the
// runtime — so an unchanged state must debounce it the same way.
func TestTickSession_EventTriggeredRepeatDoesNotRepublishKick(t *testing.T) {
	store := testStore(t)
	cfg := checkScenarioConfig(t, 4)
	seedPendingJudgeWork(t, store, "owner/repo-1")
	setCheckStatus(t, store, "owner/repo-1", "FAILURE")

	for i := range 3 {
		result, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", Trigger: TickTriggerEvent})
		if err != nil {
			t.Fatalf("TickSession(%d): %v", i, err)
		}
		if len(result.Actions) != 1 || result.Actions[0].Action != "kick" {
			t.Fatalf("tick %d actions = %+v, want kick", i, result.Actions)
		}
	}

	if got := tickEventCount(t, store, "owner/repo-1", event.TypeUserEmit); got != 1 {
		t.Fatalf("kick events = %d, want 1 for three evaluations of one unchanged state", got)
	}
}

// The heartbeat sweep is the session's normal, bounded cadence (minutes), and
// re-nudging a still-unmet state is exactly its job — so it keeps publishing
// where the event path debounces.
func TestTickSession_HeartbeatRepublishesUnchangedReviewRequired(t *testing.T) {
	store := testStore(t)
	cfg := checkScenarioConfig(t, 4)
	seedPendingJudgeWork(t, store, "owner/repo-1")

	for i := range 2 {
		if _, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", Trigger: TickTriggerHeartbeat}); err != nil {
			t.Fatalf("TickSession(%d): %v", i, err)
		}
	}

	if got := tickEventCount(t, store, "owner/repo-1", event.TypeTickReviewRequired); got != 2 {
		t.Fatalf("review_required events = %d, want 2 (one per heartbeat sweep)", got)
	}
}

// A manual `plect tick` is a human asking for the signal now; it must not be
// swallowed by a debounce meant for machine-driven re-evaluation.
func TestTickSession_ManualTickRepublishesUnchangedReviewRequired(t *testing.T) {
	store := testStore(t)
	cfg := checkScenarioConfig(t, 4)
	seedPendingJudgeWork(t, store, "owner/repo-1")

	for i := range 2 {
		if _, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1"}); err != nil {
			t.Fatalf("TickSession(%d): %v", i, err)
		}
	}

	if got := tickEventCount(t, store, "owner/repo-1", event.TypeTickReviewRequired); got != 2 {
		t.Fatalf("review_required events = %d, want 2 (a manual tick always publishes)", got)
	}
}
