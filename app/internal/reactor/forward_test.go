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

// startForwarder starts f.run and blocks until its first drain has run, so
// callers need no fixed sleep despite SQLite's variable first-touch cost —
// mirroring startReactor.
func startForwarder(t *testing.T, f *sessionForwarder) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.run(ctx); close(done) }()
	waitForFirstDrain(t, f.log, f.session)
	return func() {
		cancel()
		<-done
	}
}

// waitForFirstDrain polls for forwardConsumer's cursor to exist: drain
// commits it every pass (even unchanged), so its presence means the
// forwarder's first drain has completed.
func waitForFirstDrain(t *testing.T, log *eventlog.Store, session string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !log.HasCursor(session, forwardConsumer) {
		if time.Now().After(deadline) {
			t.Fatal("forwarder never completed its first drain")
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

// TestSessionForwarder_SkipsHistoryTheReactorAlreadyHandled proves the
// forwarder never replays events the session's own reactor already drained
// while up: it starts no earlier than reactorConsumer's own persisted
// cursor, only an event past that position is relayed.
func TestSessionForwarder_SkipsHistoryTheReactorAlreadyHandled(t *testing.T) {
	f, _, log := newTestForwarder(t)
	log.Append(event.Event{SessionName: "down-session", ID: "already-handled", Type: "resource.updated", Direction: event.Inbound})
	_, seqs, next, err := log.List("down-session", 0, event.Filter{})
	if err != nil || len(seqs) != 1 {
		t.Fatalf("seed history: %v (seqs=%v)", err, seqs)
	}
	if err := log.CommitCursor("down-session", reactorConsumer, next); err != nil {
		t.Fatalf("seed reactor cursor: %v", err)
	}

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

	log.Append(event.Event{SessionName: "down-session", ID: "after-reactor-cursor", Type: "resource.updated", Direction: event.Inbound})

	got := waitForwardCalls(t, &mu, &calls, 1)
	if len(got) != 1 || got[0].ID != "after-reactor-cursor" {
		t.Fatalf("relayed events = %+v, want exactly one (after-reactor-cursor)", got)
	}
}

// TestSessionForwarder_DoesNotDropAnEventArrivingBeforeItsFirstDrain: an
// event arriving in the gap between the down transition and this goroutine's
// own scheduling must still be relayed once it does start.
func TestSessionForwarder_DoesNotDropAnEventArrivingBeforeItsFirstDrain(t *testing.T) {
	f, _, log := newTestForwarder(t)
	log.Append(event.Event{SessionName: "down-session", ID: "already-handled", Type: "resource.updated", Direction: event.Inbound})
	_, _, afterHandled, err := log.List("down-session", 0, event.Filter{})
	if err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if err := log.CommitCursor("down-session", reactorConsumer, afterHandled); err != nil {
		t.Fatalf("seed reactor cursor: %v", err)
	}
	// Arrives after the down transition (reactorConsumer is already frozen)
	// but before the forwarder goroutine below ever runs.
	log.Append(event.Event{SessionName: "down-session", ID: "gap-arrival", Type: "resource.updated", Direction: event.Inbound})

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

	got := waitForwardCalls(t, &mu, &calls, 1)
	if len(got) != 1 || got[0].ID != "gap-arrival" {
		t.Fatalf("relayed events = %+v, want exactly one (gap-arrival), not dropped", got)
	}
}

// TestSessionForwarder_DownUpDownCycleDoesNotReplayTheUpPeriod: a
// forwardConsumer cursor left stale from an earlier down period must not
// replay a later up period's already-handled events as newly arrived.
func TestSessionForwarder_DownUpDownCycleDoesNotReplayTheUpPeriod(t *testing.T) {
	f, st, log := newTestForwarder(t)
	var mu sync.Mutex
	var calls []event.Event
	f.forwardFn = func(_ *config.Config, _ *state.Store, _ string, ev event.Event) (bool, error) {
		mu.Lock()
		calls = append(calls, ev)
		mu.Unlock()
		return true, nil
	}
	stop := startForwarder(t, f)
	log.Append(event.Event{SessionName: "down-session", ID: "first-down-period", Type: "resource.updated", Direction: event.Inbound})
	waitForwardCalls(t, &mu, &calls, 1)
	stop() // simulates the session being brought up: the supervisor would cancel this goroutine

	// The session is up: its own reactor drains this event normally (not
	// through the forwarder), advancing reactorConsumer past it.
	log.Append(event.Event{SessionName: "down-session", ID: "handled-while-up", Type: "resource.updated", Direction: event.Inbound})
	_, _, afterUpPeriod, err := log.List("down-session", 0, event.Filter{})
	if err != nil {
		t.Fatalf("read log tail: %v", err)
	}
	if err := log.CommitCursor("down-session", reactorConsumer, afterUpPeriod); err != nil {
		t.Fatalf("advance reactor cursor past the up period: %v", err)
	}

	// Down again: a fresh forwarder (as the supervisor would start).
	f2 := &sessionForwarder{session: f.session, cfg: f.cfg, state: st, log: log, hub: f.hub}
	f2.forwardFn = f.forwardFn
	stop2 := startForwarder(t, f2)
	defer stop2()
	log.Append(event.Event{SessionName: "down-session", ID: "second-down-period", Type: "resource.updated", Direction: event.Inbound})

	got := waitForwardCalls(t, &mu, &calls, 2)
	if len(got) != 2 || got[1].ID != "second-down-period" {
		t.Fatalf("relayed events = %+v, want [first-down-period second-down-period], up-period event not replayed", got)
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
