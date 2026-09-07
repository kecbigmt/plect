package reactor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// newTestForwarder builds a sessionForwarder over a down (no produced
// run-scoped node), not-destroyed session, on a fast-poll hub so tests don't
// wait on the production 500ms poll interval — mirroring newTestReactor.
func newTestForwarder(t *testing.T) (*sessionForwarder, *state.Store, *eventlog.Store) {
	t.Helper()
	dir := t.TempDir()
	log := eventlog.NewStore(dir)
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)
	st := state.NewStore(dir)
	if err := st.Put(&domain.Session{Name: "down-session"}); err != nil {
		t.Fatal(err)
	}
	f := &sessionForwarder{
		session: "down-session",
		cfg:     &config.Config{},
		state:   st,
		log:     log,
		hub:     hub,
	}
	return f, st, log
}

// startForwarder starts f.run and blocks until its cursor is seeded, so
// callers need no fixed sleep despite SQLite's variable first-touch cost —
// mirroring startReactor.
func startForwarder(t *testing.T, f *sessionForwarder) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.run(ctx); close(done) }()
	waitForForwardCursorSeed(t, f.log, f.session)
	return func() {
		cancel()
		<-done
	}
}

func waitForForwardCursorSeed(t *testing.T, log *eventlog.Store, session string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !log.HasCursor(session, forwardConsumer) {
		if time.Now().After(deadline) {
			t.Fatal("forwarder never seeded its cursor")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForwardCalls polls until at least n forwardFn invocations have been
// recorded, or fails the test.
func waitForwardCalls(t *testing.T, mu *sync.Mutex, calls *[]event.Event, n int) []event.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := len(*calls)
		mu.Unlock()
		if got >= n {
			mu.Lock()
			defer mu.Unlock()
			out := make([]event.Event, len(*calls))
			copy(out, *calls)
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("forwardFn invoked %d time(s) in 2s, want at least %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertNeverForwarded sleeps briefly and asserts forwardFn was never
// invoked — used to prove a non-Inbound event is not relayed.
func assertNeverForwarded(t *testing.T, mu *sync.Mutex, calls *[]event.Event) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 0 {
		t.Fatalf("forwardFn invoked %+v, want none", *calls)
	}
}

func TestSessionForwarder_RelaysNewInboundEvent(t *testing.T) {
	f, _, log := newTestForwarder(t)
	var mu sync.Mutex
	var calls []event.Event
	f.forwardFn = func(_ *config.Config, _ *state.Store, session string, ev event.Event) (bool, error) {
		if session != "down-session" {
			t.Errorf("unexpected session: %q", session)
		}
		mu.Lock()
		calls = append(calls, ev)
		mu.Unlock()
		return true, nil
	}
	stop := startForwarder(t, f)
	defer stop()

	log.Append(event.Event{SessionName: "down-session", ID: "ev-1", Type: "resource.updated", Direction: event.Inbound})

	got := waitForwardCalls(t, &mu, &calls, 1)
	if got[0].ID != "ev-1" {
		t.Fatalf("relayed event = %+v, want ev-1", got[0])
	}
}

func TestSessionForwarder_IgnoresNonInboundEvents(t *testing.T) {
	f, _, log := newTestForwarder(t)
	var mu sync.Mutex
	var calls []event.Event
	f.forwardFn = func(_ *config.Config, _ *state.Store, _ string, ev event.Event) (bool, error) {
		mu.Lock()
		calls = append(calls, ev)
		mu.Unlock()
		return true, nil
	}
	stop := startForwarder(t, f)
	defer stop()

	log.Append(event.Event{SessionName: "down-session", Type: "resource.updated", Direction: event.Internal})
	log.Append(event.Event{SessionName: "down-session", Type: "resource.updated", Direction: event.Outbound})
	assertNeverForwarded(t, &mu, &calls)
}

// TestSessionForwarder_SeedsCursorAtTailSkippingPriorHistory proves the
// forwarder never replays a session's pre-existing history as newly-arrived
// forwards on its first start (the same birth-event rationale reactor.go's
// own seedCursor documents): only an Inbound event appended after this
// forwarder's first start is relayed.
func TestSessionForwarder_SeedsCursorAtTailSkippingPriorHistory(t *testing.T) {
	f, _, log := newTestForwarder(t)
	log.Append(event.Event{SessionName: "down-session", ID: "pre-existing", Type: "resource.updated", Direction: event.Inbound})

	var mu sync.Mutex
	var calls []event.Event
	f.forwardFn = func(_ *config.Config, _ *state.Store, _ string, ev event.Event) (bool, error) {
		mu.Lock()
		calls = append(calls, ev)
		mu.Unlock()
		return true, nil
	}
	stop := startForwarder(t, f)
	defer stop()

	log.Append(event.Event{SessionName: "down-session", ID: "after-start", Type: "resource.updated", Direction: event.Inbound})

	got := waitForwardCalls(t, &mu, &calls, 1)
	if len(got) != 1 || got[0].ID != "after-start" {
		t.Fatalf("relayed events = %+v, want exactly one (after-start), no pre-existing history replayed", got)
	}
}

// TestSessionForwarder_RestartAfterAFailureRetriesFromLastCommittedEvent
// proves a relay failure leaves the cursor before the failed event, so a
// fresh forwarder (as the supervisor would start after a resident restart)
// retries it rather than skipping it — mirroring
// dispatch.TestDispatcher_ReplaysFromCursorAcrossRestart's equivalent proof
// for the sibling per-event-commit follower.
func TestSessionForwarder_RestartAfterAFailureRetriesFromLastCommittedEvent(t *testing.T) {
	f, _, log := newTestForwarder(t)
	f.forwardFn = func(_ *config.Config, _ *state.Store, _ string, _ event.Event) (bool, error) {
		return false, context.DeadlineExceeded
	}
	stop := startForwarder(t, f)
	log.Append(event.Event{SessionName: "down-session", ID: "ev-1", Type: "resource.updated", Direction: event.Inbound})
	time.Sleep(150 * time.Millisecond) // let the failing drain attempt run at least once
	stop()

	var mu sync.Mutex
	var calls []event.Event
	f2 := &sessionForwarder{session: f.session, cfg: f.cfg, state: f.state, log: f.log, hub: f.hub}
	f2.forwardFn = func(_ *config.Config, _ *state.Store, _ string, ev event.Event) (bool, error) {
		mu.Lock()
		calls = append(calls, ev)
		mu.Unlock()
		return true, nil
	}
	stop2 := startForwarder(t, f2)
	defer stop2()

	got := waitForwardCalls(t, &mu, &calls, 1)
	if got[0].ID != "ev-1" {
		t.Fatalf("relayed event after restart = %+v, want ev-1 (not skipped past by the failed attempt)", got)
	}
}
