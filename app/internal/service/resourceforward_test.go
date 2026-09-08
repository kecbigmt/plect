package service

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// seedDownSession puts a session with no run-scoped node at all (down, not
// destroyed — resourceforward's own tests never need it up) and, when
// parent is non-empty, parents it to that session.
func seedDownSession(t *testing.T, store *state.Store, name, resourceID, parent string) {
	t.Helper()
	now := time.Now()
	if err := store.Put(&domain.Session{
		Name: name, ResourceID: resourceID, ParentSession: parent, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
}

// downSessionEvent is the shape a resource watcher/observer publishes
// directly to a down session's own log: an Inbound event naming the
// resource it fired for.
func downSessionEvent(id string) event.Event {
	return event.Event{
		ID:        id,
		Type:      "resource.updated",
		Source:    "resource-observer",
		Direction: event.Inbound,
		Summary:   "a reviewer requested changes",
		Metadata:  map[string]string{"url": "https://example.test/resource/1/review/9"},
	}
}

func TestForwardDownSessionEvent_RelaysToNearestLiveAncestorCarryingOriginalFacts(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedDownSession(t, store, "ancestor", "", "")
	seedDownSession(t, store, "child", "https://example.test/resource/1", "ancestor")

	forwarded, err := ForwardDownSessionEvent(cfg, store, "child", downSessionEvent("01ARZ3NDEKTSV4RRFFQ69G5FAV"))
	if err != nil {
		t.Fatalf("ForwardDownSessionEvent: %v", err)
	}
	if !forwarded {
		t.Fatal("did not relay to a live ancestor")
	}

	evs := listEvents(t, store, "ancestor", event.TypeResourceForwarded)
	if len(evs) != 1 {
		t.Fatalf("forwarded events on ancestor = %+v, want exactly one", evs)
	}
	ev := evs[0]
	if ev.Metadata[event.MetaOriginSession] != "child" {
		t.Fatalf("origin_session metadata = %q, want the down child", ev.Metadata[event.MetaOriginSession])
	}
	if ev.Metadata["resource"] != "https://example.test/resource/1" {
		t.Fatalf("resource metadata = %q, want the origin's ResourceID", ev.Metadata["resource"])
	}
	if ev.Metadata["forwarded_type"] != "resource.updated" {
		t.Fatalf("forwarded_type metadata = %q, want the original event's Type", ev.Metadata["forwarded_type"])
	}
	if ev.Metadata["forwarded_url"] != "https://example.test/resource/1/review/9" {
		t.Fatalf("forwarded_url metadata = %q, want the original event's metadata url", ev.Metadata["forwarded_url"])
	}
	if want := "a reviewer requested changes (from child)"; ev.Summary != want {
		t.Fatalf("summary = %q, want %q", ev.Summary, want)
	}
}

func TestForwardDownSessionEvent_DedupsTheSameOriginalEventWithinTargetHistory(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedDownSession(t, store, "ancestor", "", "")
	seedDownSession(t, store, "child", "https://example.test/resource/1", "ancestor")
	ev := downSessionEvent("01ARZ3NDEKTSV4RRFFQ69G5FAV")

	if forwarded, err := ForwardDownSessionEvent(cfg, store, "child", ev); err != nil || !forwarded {
		t.Fatalf("first relay: forwarded=%v err=%v", forwarded, err)
	}
	// A crash between a successful relay and the caller's own cursor commit
	// must not double-relay the same original event once retried.
	forwarded, err := ForwardDownSessionEvent(cfg, store, "child", ev)
	if err != nil {
		t.Fatalf("second relay: %v", err)
	}
	if forwarded {
		t.Fatal("re-relayed an already-forwarded event")
	}
	if got := listEvents(t, store, "ancestor", event.TypeResourceForwarded); len(got) != 1 {
		t.Fatalf("forwarded events after dedup relay = %+v, want still exactly one", got)
	}
}

func TestForwardDownSessionEvent_NoLiveAncestorSkipsWithoutError(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedDownSession(t, store, "child", "https://example.test/resource/1", "") // no parent at all

	forwarded, err := ForwardDownSessionEvent(cfg, store, "child", downSessionEvent("01ARZ3NDEKTSV4RRFFQ69G5FAV"))
	if err != nil {
		t.Fatalf("ForwardDownSessionEvent: %v", err)
	}
	if forwarded {
		t.Fatal("relayed with no live ancestor to relay to")
	}
	if got := listEvents(t, store, "child", event.TypeResourceForwarded); len(got) != 0 {
		t.Fatalf("forwarded events on origin = %+v, want none: a root session keeps today's behaviour (no push)", got)
	}
}
