package sessionhub

import (
	"fmt"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/contracts/event"
)

func newFastRegistry(store *eventlog.Store) *Registry {
	return NewRegistry(store, WithPollInterval(2*time.Millisecond))
}

func recvFrame(t *testing.T, sub *FrameSub) Frame {
	t.Helper()
	select {
	case f, ok := <-sub.Frames():
		if !ok {
			t.Fatal("subscription closed unexpectedly")
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame received")
		return Frame{}
	}
}

func assertNoFrame(t *testing.T, sub *FrameSub) {
	t.Helper()
	select {
	case f, ok := <-sub.Frames():
		if ok {
			t.Fatalf("unexpected frame: %+v", f.Event)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRegistry_DeliversLiveFramesAfterTail(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	// Pre-existing history: the reader seeds to the tail, so this is NOT broadcast.
	store.Append(event.Event{SessionName: "o/r-1", Type: "user.note", Body: "old", Direction: event.Internal})
	reg := newFastRegistry(store)
	defer reg.Close()
	sub := reg.SubscribeFrames("o/r-1")
	defer sub.Close()

	if sub.Start() == 0 {
		t.Error("boundary should be the log tail (history exists), not 0")
	}
	stored, off, next, _ := store.Append(event.Event{SessionName: "o/r-1", Type: "user.note", Body: "new", Direction: event.Internal})
	f := recvFrame(t, sub)
	if f.Event.ID != stored.ID || f.Event.Body != "new" {
		t.Errorf("frame = %+v, want the new event (old must not be re-broadcast)", f.Event)
	}
	if f.Start != off || f.Resume != next {
		t.Errorf("frame offsets = (start %d, resume %d), want (%d, %d)", f.Start, f.Resume, off, next)
	}
	assertNoFrame(t, sub) // the old event is the subscriber's own catch-up, never live
}

func TestRegistry_MultipleSubscribersShareOneReader(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	a := reg.SubscribeFrames("o/r-1")
	b := reg.SubscribeFrames("o/r-1")
	defer a.Close()
	defer b.Close()

	reg.mu.Lock()
	readers, refs := len(reg.readers), reg.readers["o/r-1"].refs
	reg.mu.Unlock()
	if readers != 1 || refs != 2 {
		t.Fatalf("want one reader with refs=2, got readers=%d refs=%d", readers, refs)
	}
	store.Append(event.Event{SessionName: "o/r-1", Type: "x", Body: "e", Direction: event.Internal})
	if recvFrame(t, a).Event.Body != "e" || recvFrame(t, b).Event.Body != "e" {
		t.Error("both subscribers should receive the event from the shared reader")
	}
}

func TestRegistry_LastUnsubscribeTearsDownReader(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	a := reg.SubscribeFrames("o/r-1")
	b := reg.SubscribeFrames("o/r-1")

	a.Close()
	reg.mu.Lock()
	_, alive := reg.readers["o/r-1"]
	reg.mu.Unlock()
	if !alive {
		t.Fatal("reader gone while a subscriber remains")
	}
	b.Close()
	reg.mu.Lock()
	_, gone := reg.readers["o/r-1"]
	reg.mu.Unlock()
	if gone {
		t.Fatal("reader should be torn down on the last unsubscribe")
	}
	c := reg.SubscribeFrames("o/r-1")
	defer c.Close()
	reg.mu.Lock()
	_, recreated := reg.readers["o/r-1"]
	reg.mu.Unlock()
	if !recreated {
		t.Fatal("reader should be recreated on re-subscribe")
	}
}

func TestRegistry_SlowConsumerDroppedWithoutStallingReader(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	slow := reg.SubscribeFrames("o/r-1") // never drained → overflows
	fast := reg.SubscribeFrames("o/r-1")
	defer fast.Close()

	const n = frameBuffer + 10
	done := make(chan int, 1)
	go func() {
		c := 0
		for range fast.Frames() {
			if c++; c == n {
				done <- c
				return
			}
		}
		done <- c
	}()
	for range n {
		store.Append(event.Event{SessionName: "o/r-1", Type: "x", Direction: event.Internal})
	}
	select {
	case c := <-done:
		if c != n {
			t.Errorf("fast consumer got %d, want %d", c, n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fast consumer stalled — a slow consumer must not block the shared reader")
	}
	// The slow consumer's channel was closed on overflow (drain to the close).
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-slow.Frames():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("slow consumer channel never closed on overflow")
		}
	}
}

func TestRegistry_ConcurrentAppendsNoGapNoDup(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	sub := reg.SubscribeFrames("o/r-1")
	defer sub.Close()

	const n = 50
	go func() {
		for i := range n {
			store.Append(event.Event{SessionName: "o/r-1", Type: "x", Body: fmt.Sprint(i), Direction: event.Internal})
		}
	}()
	seen := make(map[string]bool, n)
	deadline := time.After(3 * time.Second)
	for len(seen) < n {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				t.Fatal("subscription closed before all events arrived")
			}
			if seen[f.Event.ID] {
				t.Fatalf("duplicate frame for %s", f.Event.ID)
			}
			seen[f.Event.ID] = true
		case <-deadline:
			t.Fatalf("only %d/%d frames delivered", len(seen), n)
		}
	}
}

func TestRegistry_SessionsAreIndependent(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	a := reg.SubscribeFrames("s-a")
	b := reg.SubscribeFrames("s-b")
	defer a.Close()
	defer b.Close()

	reg.mu.Lock()
	n := len(reg.readers)
	reg.mu.Unlock()
	if n != 2 {
		t.Fatalf("want two independent readers, got %d", n)
	}
	store.Append(event.Event{SessionName: "s-a", Type: "x", Body: "for-a", Direction: event.Internal})
	if recvFrame(t, a).Event.Body != "for-a" {
		t.Error("s-a's subscriber should receive its event")
	}
	assertNoFrame(t, b) // s-b's reader must not see s-a's event
}

func TestRegistry_DoubleCloseIsSafe(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	a := reg.SubscribeFrames("o/r-1")
	b := reg.SubscribeFrames("o/r-1")
	defer b.Close()
	a.Close()
	a.Close() // idempotent: must not over-decrement and tear down b's reader
	reg.mu.Lock()
	_, alive := reg.readers["o/r-1"]
	reg.mu.Unlock()
	if !alive {
		t.Fatal("a double Close over-decremented the refcount and tore down a live reader")
	}
}

func TestRegistry_WatchSignalsOnAppend(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	wk := reg.Watch("o/r-1")
	defer wk.Close()

	store.Append(event.Event{SessionName: "o/r-1", Type: "x", Direction: event.Internal})
	select {
	case <-wk.Wake():
	case <-time.After(2 * time.Second):
		t.Fatal("no wake signalled on append")
	}
}

func TestRegistry_WatchAndFramesShareOneReader(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	defer reg.Close()
	wk := reg.Watch("o/r-1")
	defer wk.Close()
	fr := reg.SubscribeFrames("o/r-1")
	defer fr.Close()

	reg.mu.Lock()
	readers, refs := len(reg.readers), reg.readers["o/r-1"].refs
	reg.mu.Unlock()
	if readers != 1 || refs != 2 {
		t.Fatalf("a wake and a frame consumer must share one reader (refs=2); got readers=%d refs=%d", readers, refs)
	}
	// One append both delivers a frame and signals the wake.
	store.Append(event.Event{SessionName: "o/r-1", Type: "x", Body: "e", Direction: event.Internal})
	if recvFrame(t, fr).Event.Body != "e" {
		t.Error("frame consumer missed the event")
	}
	select {
	case <-wk.Wake():
	case <-time.After(2 * time.Second):
		t.Fatal("wake consumer missed the signal")
	}
}

func TestRegistry_CloseCancelsReaders(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := newFastRegistry(store)
	reg.SubscribeFrames("o/r-1") // leak a sub; Close must still tear the reader down
	reg.Close()
	reg.mu.Lock()
	n := len(reg.readers)
	reg.mu.Unlock()
	if n != 0 {
		t.Fatalf("Close should drop all readers, got %d", n)
	}
}

// Regression: the last unsubscribe used to only cancel the reader's context
// and return, without waiting for the reader goroutine to actually observe
// cancellation and exit. A caller that immediately tore down the session's
// on-disk directory (as t.TempDir() cleanup does) could race a still-running
// reader mid-poll — its flock() helper reopens the lock file with O_CREATE,
// so a poll landing after the directory's other entries were already removed
// recreates it, surfacing as an intermittent "directory not empty" cleanup
// failure. The reader must be fully joined before the last unsubscribe/Close
// returns.
func TestRegistry_LastUnsubscribeJoinsReaderGoroutine(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := NewRegistry(store, WithPollInterval(50*time.Millisecond))
	sub := reg.SubscribeFrames("o/r-1")

	reg.mu.Lock()
	r := reg.readers["o/r-1"].reader
	reg.mu.Unlock()

	sub.Close()

	select {
	case <-r.done:
	default:
		t.Fatal("Close returned before the reader goroutine exited — teardown is not joined")
	}
}

func TestRegistry_ActiveReaderSurvivesStreamRotation(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	store.Append(event.Event{SessionName: "o/r-1", Type: "user.note", Body: "old-seen", Direction: event.Internal})
	// A slow poll gives every write below a wide window to land before the reader's first check, guaranteeing the rotation is mid-flight, not already resolved.
	reg := NewRegistry(store, WithPollInterval(200*time.Millisecond))
	defer reg.Close()
	sub := reg.SubscribeFrames("o/r-1")
	defer sub.Close()

	oldTail, _, _, _ := store.Append(event.Event{SessionName: "o/r-1", Type: "user.note", Body: "old-tail", Direction: event.Internal})
	if _, err := store.NewStream("o/r-1"); err != nil {
		t.Fatal(err)
	}
	stored1, _, _, _ := store.Append(event.Event{SessionName: "o/r-1", Type: "user.note", Body: "new-first", Direction: event.Internal})
	stored2, _, _, _ := store.Append(event.Event{SessionName: "o/r-1", Type: "user.note", Body: "new-second", Direction: event.Internal})

	f0 := recvFrame(t, sub)
	if f0.Event.ID != oldTail.ID || f0.Event.Body != "old-tail" {
		t.Fatalf("first frame = %+v, want the superseded stream's own unbroadcast tail record", f0.Event)
	}
	f1 := recvFrame(t, sub)
	if f1.Event.ID != stored1.ID || f1.Event.Body != "new-first" || f1.StreamID == f0.StreamID {
		t.Fatalf("second frame = %+v (stream %s), want new-first on a distinct stream from %s", f1.Event, f1.StreamID, f0.StreamID)
	}
	f2 := recvFrame(t, sub)
	if f2.Event.ID != stored2.ID || f2.Event.Body != "new-second" || f2.StreamID != f1.StreamID {
		t.Fatalf("third frame = %+v (stream %s), want new-second on the same stream as %s", f2.Event, f2.StreamID, f1.StreamID)
	}
}

func TestRegistry_CloseJoinsReaderGoroutines(t *testing.T) {
	store := eventlog.NewStore(t.TempDir())
	reg := NewRegistry(store, WithPollInterval(50*time.Millisecond))
	// Leaking the sub (never closing it) exercises Close tearing the reader
	// down on its own, independent of any consumer unsubscribing.
	reg.SubscribeFrames("o/r-1")

	reg.mu.Lock()
	r := reg.readers["o/r-1"].reader
	reg.mu.Unlock()

	reg.Close()

	select {
	case <-r.done:
	default:
		t.Fatal("Close returned before the reader goroutine exited — teardown is not joined")
	}
}
