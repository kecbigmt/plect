package eventlog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func TestNewStore_HonorsPlectDataHomeLikeStateStore(t *testing.T) {
	override := filepath.Join(t.TempDir(), "custom-data")
	t.Setenv("PLECT_DATA_HOME", override)

	store := NewStore("")
	if got := store.Dir(); got != override {
		t.Fatalf("Dir() = %q, want %q", got, override)
	}
}

// newSessionForTest mints a fresh, live session row for session and
// returns its id.
func newSessionForTest(t *testing.T, s *Store, session string) string {
	t.Helper()
	db, err := s.dbHandle()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.PutSession(ctx, &domain.Session{Name: session, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	id, err := db.EventStreamID(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// destroyAndRecreateForTest transitions session's live row to destroyed and
// creates a fresh one under the same name, a second distinct incarnation.
func destroyAndRecreateForTest(t *testing.T, s *Store, session string) string {
	t.Helper()
	db, err := s.dbHandle()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := db.UpdateSession(ctx, session, func(sess *domain.Session) error {
		sess.Status = contract.SessionStatusDestroyed
		sess.DestroyedAt = time.Now().UTC()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return newSessionForTest(t, s, session)
}

// TestMain lets a test re-exec itself as a child appender so we can exercise the
// cross-process flock path (the in-process mutex hides it within one Store).
func TestMain(m *testing.M) {
	if dir := os.Getenv("EVENTLOG_CHILD_DIR"); dir != "" {
		session := os.Getenv("EVENTLOG_CHILD_SESSION")
		n, _ := strconv.Atoi(os.Getenv("EVENTLOG_CHILD_N"))
		s := NewStore(dir)
		for range n {
			if _, _, _, err := s.Append(event.Event{SessionName: session, Type: "test.tick", Source: "test", Direction: event.Internal}); err != nil {
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSwapChainAttempt_ReportsPreviousAndWon(t *testing.T) {
	s := NewStore(t.TempDir())
	newSessionForTest(t, s, "work1")

	previous, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|target")
	if err != nil {
		t.Fatalf("SwapChainAttempt (first): %v", err)
	}
	if previous != "" || !won {
		t.Fatalf("first swap: previous=%q won=%v, want \"\"/true", previous, won)
	}

	previous, won, err = s.SwapChainAttempt("work1", "work", "review", "cap|target")
	if err != nil {
		t.Fatalf("SwapChainAttempt (unchanged): %v", err)
	}
	if previous != "cap|target" || won {
		t.Fatalf("unchanged swap: previous=%q won=%v, want \"cap|target\"/false", previous, won)
	}

	previous, won, err = s.SwapChainAttempt("work1", "work", "review", "")
	if err != nil {
		t.Fatalf("SwapChainAttempt (clear): %v", err)
	}
	if previous != "cap|target" || !won {
		t.Fatalf("clearing swap: previous=%q won=%v, want \"cap|target\"/true", previous, won)
	}
}

func TestRevertChainAttempt_DoesNotOverwriteANewerTransition(t *testing.T) {
	s := NewStore(t.TempDir())
	newSessionForTest(t, s, "work1")

	if _, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|A"); err != nil || !won {
		t.Fatalf("claim A: won=%v err=%v", won, err)
	}
	if previous, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|B"); err != nil || !won || previous != "cap|A" {
		t.Fatalf("claim B: previous=%q won=%v err=%v", previous, won, err)
	}

	reverted, err := s.RevertChainAttempt("work1", "work", "review", "cap|A", "")
	if err != nil {
		t.Fatalf("RevertChainAttempt: %v", err)
	}
	if reverted {
		t.Fatal("reverted = true, want false: the marker had already moved past what this caller claimed")
	}

	if previous, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|B"); err != nil || won || previous != "cap|B" {
		t.Fatalf("marker after the stale revert: previous=%q won=%v err=%v, want \"cap|B\"/false/nil", previous, won, err)
	}
}

func TestClearChainAttempts_RemovesEveryMarkerForTheSession(t *testing.T) {
	s := NewStore(t.TempDir())
	id := newSessionForTest(t, s, "work1")

	if _, _, err := s.SwapChainAttempt("work1", "work", "review", "cap|A"); err != nil {
		t.Fatalf("SwapChainAttempt: %v", err)
	}
	if err := s.ClearChainAttempts(id); err != nil {
		t.Fatalf("ClearChainAttempts: %v", err)
	}
	if previous, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|A"); err != nil || !won || previous != "" {
		t.Fatalf("after clear: previous=%q won=%v err=%v, want \"\"/true/nil", previous, won, err)
	}

	if err := s.ClearChainAttempts(id); err != nil {
		t.Fatalf("ClearChainAttempts on a session with no markers: %v", err)
	}
}

func TestClearChainAttempts_DoesNotClearAReplacementIncarnation(t *testing.T) {
	s := NewStore(t.TempDir())
	oldID := newSessionForTest(t, s, "work1")
	if _, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|old"); err != nil || !won {
		t.Fatalf("old claim: won=%v err=%v", won, err)
	}
	destroyAndRecreateForTest(t, s, "work1")
	if _, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|new"); err != nil || !won {
		t.Fatalf("new claim: won=%v err=%v", won, err)
	}
	if err := s.ClearChainAttempts(oldID); err != nil {
		t.Fatal(err)
	}
	previous, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|new")
	if err != nil || won || previous != "cap|new" {
		t.Fatalf("replacement marker after old cleanup = previous %q won %v err %v", previous, won, err)
	}
}

func TestSwapChainAttempt_ReusesTheChainKeyAfterClear(t *testing.T) {
	s := NewStore(t.TempDir())
	id := newSessionForTest(t, s, "work1")

	if _, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|A"); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if err := s.ClearChainAttempts(id); err != nil {
		t.Fatalf("ClearChainAttempts: %v", err)
	}
	if _, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|A"); err != nil || !won {
		t.Fatalf("rewrite after clear: won=%v err=%v", won, err)
	}

	previous, won, err := s.SwapChainAttempt("work1", "work", "review", "cap|A")
	if err != nil {
		t.Fatalf("unchanged claim: %v", err)
	}
	if previous != "cap|A" || won {
		t.Fatalf("unchanged claim: previous=%q won=%v, want \"cap|A\"/false", previous, won)
	}
}

func TestSwapChainAttempt_ConcurrentIdenticalSwapsExactlyOneWins(t *testing.T) {
	s := NewStore(t.TempDir())
	newSessionForTest(t, s, "work1")
	const n = 20

	var wg sync.WaitGroup
	won := make([]bool, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, w, err := s.SwapChainAttempt("work1", "work", "review", "cap|target")
			won[i], errs[i] = w, err
		}(i)
	}
	wg.Wait()

	wins := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("SwapChainAttempt[%d]: %v", i, err)
		}
		if won[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 of %d concurrent identical swaps", wins, n)
	}
}

func TestAppendAndList(t *testing.T) {
	s := NewStore(t.TempDir())
	const session = "octocat/hello-world-42"

	want := []struct{ typ, src string }{
		{"acme.ci_status", "acme"},
		{"widget.message", "widget"},
		{"gadget.reply", "gadget"},
	}
	var offsets []int64
	for _, w := range want {
		_, off, next, err := s.Append(event.Event{SessionName: session, Type: w.typ, Source: w.src, Direction: event.Internal})
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if len(offsets) > 0 && off <= offsets[len(offsets)-1] {
			t.Fatalf("offset not increasing: %d after %d", off, offsets[len(offsets)-1])
		}
		offsets = append(offsets, off)
		_ = next
	}

	all, offs, _, err := s.List(session, 0, event.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	for i := range all {
		if all[i].ID == "" || all[i].Time.IsZero() {
			t.Errorf("event %d missing id/time", i)
		}
		if offs[i] != offsets[i] {
			t.Errorf("list offset[%d]=%d, append said %d", i, offs[i], offsets[i])
		}
	}

	// type glob filter
	gh, _, _, err := s.List(session, 0, event.Filter{Types: []string{"acme.*"}})
	if err != nil || len(gh) != 1 || gh[0].Type != "acme.ci_status" {
		t.Fatalf("acme.* filter: %v len=%d", err, len(gh))
	}

	// since-offset resumes after the first record
	rest, _, _, err := s.List(session, offsets[1], event.Filter{})
	if err != nil || len(rest) != 2 {
		t.Fatalf("since filter: %v len=%d", err, len(rest))
	}
}

func TestSessionsEnumeratesTouchedStreams(t *testing.T) {
	s := NewStore(t.TempDir())

	// No stream touched yet → empty, no error.
	if names, err := s.Sessions(); err != nil || len(names) != 0 {
		t.Fatalf("empty store: names=%v err=%v", names, err)
	}

	for _, name := range []string{"octocat/hello-world-42", "owner/repo-1", "owner/repo-1+tag"} {
		if _, _, _, err := s.Append(event.Event{SessionName: name, Type: "t", Direction: event.Internal}); err != nil {
			t.Fatalf("append %s: %v", name, err)
		}
	}
	got, err := s.Sessions()
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	// Sorted, including names containing "/" (an opaque session name, not
	// interpreted as owner/repo).
	want := []string{"octocat/hello-world-42", "owner/repo-1", "owner/repo-1+tag"}
	if len(got) != len(want) {
		t.Fatalf("sessions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sessions[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ListAcross is the merge primitive behind the subtree cross-session view: it
// merges the named sessions' events in id order, applying the filter's other
// selectors. Sessions not named are excluded.
func TestListAcrossMergesNamedSessions(t *testing.T) {
	s := NewStore(t.TempDir())

	seq := []struct{ session, summary string }{
		{"root", "r1"}, {"work", "w1"}, {"outside", "x1"}, {"work", "w2"}, {"root", "r2"},
	}
	for _, e := range seq {
		if _, _, _, err := s.Append(event.Event{SessionName: e.session, Type: "t", Source: "test", Summary: e.summary, Direction: event.Internal}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := s.ListAcross([]string{"root", "work"}, event.Filter{})
	if err != nil {
		t.Fatalf("listacross: %v", err)
	}
	want := []string{"r1", "w1", "w2", "r2"}
	if len(got) != len(want) {
		t.Fatalf("across = %v, want %v", summaries(got), want)
	}
	for i := range want {
		if got[i].Summary != want[i] {
			t.Fatalf("across[%d] = %q, want %q", i, got[i].Summary, want[i])
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Fatalf("ids not ascending: %q then %q", got[i-1].ID, got[i].ID)
		}
	}

	// An empty name set yields no events, not an error (an empty subtree is valid).
	if evs, err := s.ListAcross(nil, event.Filter{}); err != nil || len(evs) != 0 {
		t.Fatalf("empty name set = (%v, %v), want (nil, nil)", summaries(evs), err)
	}
}

func TestConcurrentAppendMultiProcess(t *testing.T) {
	dir := t.TempDir()
	const session = "owner/repo-7+tag"
	const procs, per = 5, 20

	var cmds []*exec.Cmd
	for i := range procs {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(),
			"EVENTLOG_CHILD_DIR="+dir,
			"EVENTLOG_CHILD_SESSION="+session,
			"EVENTLOG_CHILD_N="+strconv.Itoa(per),
		)
		if err := c.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
		cmds = append(cmds, c)
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("child %d failed: %v", i, err)
		}
	}

	s := NewStore(dir)
	evs, offs, _, err := s.List(session, 0, event.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != procs*per {
		t.Fatalf("got %d events, want %d (lost/torn appends?)", len(evs), procs*per)
	}
	ids := map[string]bool{}
	var prev int64 = -1
	for i := range evs {
		if evs[i].ID == "" || ids[evs[i].ID] {
			t.Fatalf("duplicate/empty id at %d: %q", i, evs[i].ID)
		}
		ids[evs[i].ID] = true
		if offs[i] <= prev {
			t.Fatalf("offset not strictly increasing at %d: %d after %d", i, offs[i], prev)
		}
		prev = offs[i]
	}
}

func TestTailReturnsLastN(t *testing.T) {
	s := NewStore(t.TempDir())
	const session = "owner/repo-9"
	for i := range 25 {
		if _, _, _, err := s.Append(event.Event{SessionName: session, Type: "t", Summary: strconv.Itoa(i), Direction: event.Internal}); err != nil {
			t.Fatal(err)
		}
	}

	// limit < total: the last N in append order.
	got, err := s.Tail(session, event.Filter{}, 10)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 10 || got[0].Summary != "15" || got[9].Summary != "24" {
		t.Fatalf("tail(10) = %d events, first=%q last=%q", len(got), summaryAt(got, 0), summaryAt(got, len(got)-1))
	}

	// limit > total: everything.
	all, err := s.Tail(session, event.Filter{}, 100)
	if err != nil || len(all) != 25 {
		t.Fatalf("tail(100) = %d events (err=%v), want 25", len(all), err)
	}

	// limit <= 0: everything.
	zero, err := s.Tail(session, event.Filter{}, 0)
	if err != nil || len(zero) != 25 {
		t.Fatalf("tail(0) = %d events, want all 25", len(zero))
	}

	// missing session: empty, no error.
	none, err := s.Tail("never/created-1", event.Filter{}, 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("tail(missing) = %d events (err=%v)", len(none), err)
	}
}

func TestTailOffset(t *testing.T) {
	s := NewStore(t.TempDir())
	const session = "owner/repo-9"
	for i := range 25 {
		if _, _, _, err := s.Append(event.Event{SessionName: session, Type: "t", Summary: strconv.Itoa(i), Direction: event.Internal}); err != nil {
			t.Fatal(err)
		}
	}

	// n < total: the offset replays exactly the last n records (15..24).
	off, err := s.TailOffset(session, event.Filter{}, 10)
	if err != nil {
		t.Fatalf("tailOffset: %v", err)
	}
	evs, _, _, err := s.List(session, off, event.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 10 || evs[0].Summary != "15" || evs[9].Summary != "24" {
		t.Fatalf("from tail offset got %d events, first=%q last=%q", len(evs), summaryAt(evs, 0), summaryAt(evs, len(evs)-1))
	}

	// n >= total and n <= 0: replay from the head (offset 0).
	if off, _ := s.TailOffset(session, event.Filter{}, 100); off != 0 {
		t.Errorf("tailOffset(100) = %d, want 0 (replay all)", off)
	}
	if off, _ := s.TailOffset(session, event.Filter{}, 0); off != 0 {
		t.Errorf("tailOffset(0) = %d, want 0", off)
	}

	// missing session: 0, no error.
	if off, err := s.TailOffset("never/created-1", event.Filter{}, 5); err != nil || off != 0 {
		t.Errorf("tailOffset(missing) = %d (err=%v)", off, err)
	}
}

func TestTailAppliesFilterToTheRing(t *testing.T) {
	s := NewStore(t.TempDir())
	const session = "owner/repo-9"
	// Interleave two types so the newest few records are NOT all "keep".
	for i := range 20 {
		typ := "skip"
		if i%2 == 0 {
			typ = "keep"
		}
		if _, _, _, err := s.Append(event.Event{SessionName: session, Type: typ, Summary: strconv.Itoa(i), Direction: event.Internal}); err != nil {
			t.Fatal(err)
		}
	}

	// The ring must keep the last N *matching* events, not the last N then
	// filter (which would drop most of them). "keep" is on even i: 0,2,…,18.
	got, err := s.Tail(session, event.Filter{Types: []string{"keep"}}, 3)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 3 || got[0].Summary != "14" || got[2].Summary != "18" {
		t.Fatalf("filtered tail(3) = %v", summaries(got))
	}
}

func TestGen(t *testing.T) {
	s := NewStore(t.TempDir())
	const session = "owner/repo-9"

	// No log yet → no generation, no error.
	if g, err := s.StreamID(session); err != nil || g != "" {
		t.Fatalf("gen of empty log = %q (err=%v), want empty", g, err)
	}

	if _, _, _, err := s.Append(event.Event{SessionName: session, Type: "a", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}
	g1, err := s.StreamID(session)
	if err != nil || g1 == "" {
		t.Fatalf("gen after first append = %q (err=%v), want non-empty", g1, err)
	}

	// Stable across further appends (no rotation).
	if _, _, _, err := s.Append(event.Event{SessionName: session, Type: "b", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}
	g2, err := s.StreamID(session)
	if err != nil || g2 != g1 {
		t.Fatalf("gen changed across appends: %q → %q", g1, g2)
	}
}

func summaries(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i := range evs {
		out[i] = evs[i].Summary
	}
	return out
}

func summaryAt(evs []event.Event, i int) string {
	if i < 0 || i >= len(evs) {
		return ""
	}
	return evs[i].Summary
}

func TestCursorRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	const session, consumer = "o/r-1", "delivery"
	newSessionForTest(t, s, session)
	if off, err := s.ReadCursor(session, consumer); err != nil || off != 0 {
		t.Fatalf("missing cursor should be 0: off=%d err=%v", off, err)
	}
	if err := s.CommitCursor(session, consumer, 4096); err != nil {
		t.Fatalf("commit: %v", err)
	}
	off, err := s.ReadCursor(session, consumer)
	if err != nil || off != 4096 {
		t.Fatalf("read cursor: off=%d err=%v", off, err)
	}
}

func TestHasCursor(t *testing.T) {
	s := NewStore(t.TempDir())
	const session, consumer = "o/r-1", "delivery"
	newSessionForTest(t, s, session)
	if s.HasCursor(session, consumer) {
		t.Error("HasCursor true before any commit")
	}
	// A committed 0 must read as "has a cursor" (distinct from never-committed).
	if err := s.CommitCursor(session, consumer, 0); err != nil {
		t.Fatal(err)
	}
	if !s.HasCursor(session, consumer) {
		t.Error("HasCursor false after committing 0")
	}
}

func TestFollowDeliversNewEvents(t *testing.T) {
	s := NewStore(t.TempDir())
	const session = "o/r-1"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	got := make(chan event.Event, 4)
	go func() { _ = s.Follow(ctx, session, 0, func(ev event.Event, _ int64) { got <- ev }) }()

	time.Sleep(50 * time.Millisecond)
	if _, _, _, err := s.Append(event.Event{SessionName: session, Type: "live", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-got:
		if ev.Type != "live" {
			t.Fatalf("got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not deliver the appended event")
	}
}

func TestReadFromStream_TraversesEveryIntermediateIncarnation(t *testing.T) {
	store := NewStore(t.TempDir())
	const session = "o/r-1"

	stream1 := newSessionForTest(t, store, session)
	if _, _, _, err := store.Append(event.Event{SessionName: session, Type: "user.note", Body: "s1", Direction: event.Internal}); err != nil {
		t.Fatalf("append to stream 1: %v", err)
	}

	destroyAndRecreateForTest(t, store, session)
	if _, _, _, err := store.Append(event.Event{SessionName: session, Type: "user.note", Body: "s2", Direction: event.Internal}); err != nil {
		t.Fatalf("append to stream 2: %v", err)
	}

	destroyAndRecreateForTest(t, store, session)
	if _, _, _, err := store.Append(event.Event{SessionName: session, Type: "user.note", Body: "s3", Direction: event.Internal}); err != nil {
		t.Fatalf("append to stream 3: %v", err)
	}

	var got []string
	streamID, cur := stream1, int64(2)
	for i := 0; i < 6 && len(got) < 2; i++ {
		evs, _, resolved, next, err := store.ReadFromStream(session, streamID, cur)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		for _, ev := range evs {
			got = append(got, ev.Body)
		}
		if resolved != "" {
			streamID = resolved
		}
		cur = next
	}
	if len(got) != 2 || got[0] != "s2" || got[1] != "s3" {
		t.Fatalf("delivered %v, want [s2 s3] (the intermediate incarnation must not be skipped)", got)
	}
}

func TestReadFromStream_EmptyIntermediateIncarnationDoesNotStopTheWalk(t *testing.T) {
	store := NewStore(t.TempDir())
	const session = "o/r-1"

	stream1 := newSessionForTest(t, store, session)
	if _, _, _, err := store.Append(event.Event{SessionName: session, Type: "user.note", Body: "s1", Direction: event.Internal}); err != nil {
		t.Fatalf("append to stream 1: %v", err)
	}

	destroyAndRecreateForTest(t, store, session)

	destroyAndRecreateForTest(t, store, session)
	if _, _, _, err := store.Append(event.Event{SessionName: session, Type: "user.note", Body: "s3", Direction: event.Internal}); err != nil {
		t.Fatalf("append to stream 3: %v", err)
	}

	evs, _, resolved, _, err := store.ReadFromStream(session, stream1, 2)
	if err != nil {
		t.Fatalf("ReadFromStream: %v", err)
	}
	if len(evs) != 1 || evs[0].Body != "s3" {
		t.Fatalf("evs = %v, want [s3] (the empty stream2 must not stop the walk before reaching stream3)", evs)
	}
	if resolved == stream1 {
		t.Fatal("resolved stream id did not advance past the drained incarnation")
	}
}

func TestResidentPathsShareOneConnectionPerDatabase(t *testing.T) {
	dir := t.TempDir()
	const session = "o/r-1"

	first, err := NewStore(dir).dbHandle()
	if err != nil {
		t.Fatalf("first dbHandle: %v", err)
	}

	for i := 0; i < 50; i++ {
		s := NewStore(dir)
		if _, _, _, err := s.Append(event.Event{SessionName: session, Type: "user.note", Direction: event.Internal}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if _, err := s.StreamID(session); err != nil {
			t.Fatalf("stream id %d: %v", i, err)
		}
		if _, _, _, err := s.List(session, 0, event.Filter{}); err != nil {
			t.Fatalf("list %d: %v", i, err)
		}
		if err := s.CommitCursor(session, "delivery", 1); err != nil {
			t.Fatalf("commit cursor %d: %v", i, err)
		}
	}

	current, err := NewStore(dir).dbHandle()
	if err != nil {
		t.Fatalf("final dbHandle: %v", err)
	}
	if current != first {
		t.Fatal("the shared connection changed across 50 fresh Store values over the same directory; want the same one reused throughout, not a new pool opened per call")
	}
}
