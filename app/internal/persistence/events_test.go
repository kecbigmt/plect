package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// createSessionForTest mints a fresh, live session row.
func createSessionForTest(t *testing.T, db *DB, name string) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.PutSession(context.Background(), &domain.Session{Name: name, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create session %q: %v", name, err)
	}
}

// destroyAndRecreateSessionForTest mints name's second, distinct incarnation.
func destroyAndRecreateSessionForTest(t *testing.T, db *DB, name string) {
	t.Helper()
	ctx := context.Background()
	if err := db.UpdateSession(ctx, name, func(s *domain.Session) error {
		s.Status = contract.SessionStatusDestroyed
		s.DestroyedAt = time.Now().UTC()
		return nil
	}); err != nil {
		t.Fatalf("destroy session %q: %v", name, err)
	}
	createSessionForTest(t, db, name)
}

func TestAppendEvent_AssignsIncreasingPerStreamSequence(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	createSessionForTest(t, db, session)
	createSessionForTest(t, db, "s2")

	first, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a", Source: "test", Direction: event.Internal})
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if first != 1 {
		t.Fatalf("first sequence = %d, want 1", first)
	}

	second, err := db.AppendEvent(ctx, event.Event{ID: "e2", SessionName: session, Time: time.Now().UTC(), Type: "b", Source: "test", Direction: event.Internal})
	if err != nil {
		t.Fatalf("append second: %v", err)
	}
	if second != 2 {
		t.Fatalf("second sequence = %d, want 2", second)
	}

	// A different session's incarnation starts its own sequence at 1.
	other, err := db.AppendEvent(ctx, event.Event{ID: "e3", SessionName: "s2", Time: time.Now().UTC(), Type: "a", Source: "test", Direction: event.Internal})
	if err != nil {
		t.Fatalf("append other session: %v", err)
	}
	if other != 1 {
		t.Fatalf("other session's first sequence = %d, want 1", other)
	}
}

func TestAppendEvent_RoundTripsEveryField(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	when := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	createSessionForTest(t, db, session)

	want := event.Event{
		ID:          "01JXAMPLE",
		SessionName: session,
		Time:        when,
		Type:        "widget.message",
		Source:      "widget",
		Direction:   event.Inbound,
		Summary:     "hello",
		Body:        "hello body",
		Metadata:    map[string]string{"k": "v"},
	}
	if _, err := db.AppendEvent(ctx, want); err != nil {
		t.Fatalf("append: %v", err)
	}

	got, seqs, err := db.ListEventsFrom(ctx, session, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || len(seqs) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if seqs[0] != 1 {
		t.Fatalf("sequence = %d, want 1", seqs[0])
	}
	ev := got[0]
	if ev.ID != want.ID || ev.SessionName != want.SessionName || !ev.Time.Equal(want.Time) ||
		ev.Type != want.Type || ev.Source != want.Source || ev.Direction != want.Direction ||
		ev.Summary != want.Summary || ev.Body != want.Body {
		t.Fatalf("round-tripped event = %+v, want %+v", ev, want)
	}
	if ev.Metadata["k"] != "v" {
		t.Fatalf("metadata = %+v, want k=v", ev.Metadata)
	}
}

func TestAppendEvent_EmptyMetadataRoundTripsAsNil(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	createSessionForTest(t, db, session)

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a", Source: "test", Direction: event.Internal}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _, err := db.ListEventsFrom(ctx, session, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Metadata != nil {
		t.Fatalf("metadata = %+v, want nil", got[0].Metadata)
	}
}

func TestAppendEvent_RejectsEmptyDirection(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	createSessionForTest(t, db, session)

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a"}); err == nil {
		t.Fatal("append with empty direction succeeded, want error")
	}
}

func TestAppendEvent_RejectsMissingLiveSession(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: "never/created", Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err == nil {
		t.Fatal("append to a session with no live row succeeded, want error")
	}
}

func TestListEventsFrom_ResumesAfterAGivenSequence(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	createSessionForTest(t, db, session)

	for i := range 3 {
		if _, err := db.AppendEvent(ctx, event.Event{ID: string(rune('a' + i)), SessionName: session, Time: time.Now().UTC(), Type: "t", Direction: event.Internal}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	rest, seqs, err := db.ListEventsFrom(ctx, session, 2)
	if err != nil {
		t.Fatalf("list from 2: %v", err)
	}
	if len(rest) != 2 || seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("list from 2 = %+v (seqs %v), want sequences [2 3]", rest, seqs)
	}
}

func TestListEventsFrom_MissingSessionIsEmptyNotError(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	evs, seqs, err := db.ListEventsFrom(ctx, "never/created", 0)
	if err != nil || len(evs) != 0 || len(seqs) != 0 {
		t.Fatalf("list on missing session = (%v, %v, %v), want empty/no error", evs, seqs, err)
	}
}

func TestEventStreamID_EmptyUntilCreatedThenStableAcrossAppends(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s9"

	gen, err := db.EventStreamID(ctx, session)
	if err != nil || gen != "" {
		t.Fatalf("stream id before create = %q (err=%v), want empty", gen, err)
	}

	createSessionForTest(t, db, session)
	g1, err := db.EventStreamID(ctx, session)
	if err != nil || g1 == "" {
		t.Fatalf("stream id after create = %q (err=%v), want non-empty", g1, err)
	}

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e2", SessionName: session, Time: time.Now().UTC(), Type: "b", Direction: event.Internal}); err != nil {
		t.Fatalf("append: %v", err)
	}
	g2, err := db.EventStreamID(ctx, session)
	if err != nil || g2 != g1 {
		t.Fatalf("stream id changed across appends: %q -> %q", g1, g2)
	}
}

func TestEventStreamID_DestroyAndRecreateMintsANewCurrentIncarnation(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"

	createSessionForTest(t, db, session)
	first, err := db.EventStreamID(ctx, session)
	if err != nil || first == "" {
		t.Fatalf("stream id after create: %q, err=%v", first, err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err != nil {
		t.Fatalf("append to first incarnation: %v", err)
	}

	destroyAndRecreateSessionForTest(t, db, session)
	second, err := db.EventStreamID(ctx, session)
	if err != nil {
		t.Fatalf("stream id after recreate: %v", err)
	}
	if second == first {
		t.Fatalf("second incarnation's id = %q, want distinct from first %q", second, first)
	}

	seq, err := db.AppendEvent(ctx, event.Event{ID: "e2", SessionName: session, Time: time.Now().UTC(), Type: "b", Direction: event.Internal})
	if err != nil {
		t.Fatalf("append to second incarnation: %v", err)
	}
	if seq != 1 {
		t.Fatalf("second incarnation's first sequence = %d, want 1 (its own row, not continuing the first's)", seq)
	}

	// The destroyed first incarnation's own events remain readable by its id.
	firstEvs, _, err := db.ListEventsFromStreamID(ctx, first, session, 0)
	if err != nil {
		t.Fatalf("list first incarnation's events by id: %v", err)
	}
	if len(firstEvs) != 1 || firstEvs[0].ID != "e1" {
		t.Fatalf("first incarnation's events = %+v, want exactly [e1]", firstEvs)
	}
}

// TestEventCursor_DestroyAndRecreateResetsNameBasedReaderWithoutLoss proves
// a name-based reader (ReadCursor/ListEventsFrom) picks up a recreated
// incarnation's events from its own beginning: event_cursors keys off
// session_id, so the new incarnation's fresh id has no cursor row to be
// misread as "already past" its own low sequence numbers.
func TestEventCursor_DestroyAndRecreateResetsNameBasedReaderWithoutLoss(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session, cursorName = "s1", "delivery"

	createSessionForTest(t, db, session)
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err != nil {
		t.Fatalf("append e1: %v", err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e2", SessionName: session, Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err != nil {
		t.Fatalf("append e2: %v", err)
	}
	// A reader caught up to the first incarnation's last event.
	if err := db.SetEventCursor(ctx, session, cursorName, 2); err != nil {
		t.Fatalf("set cursor: %v", err)
	}

	destroyAndRecreateSessionForTest(t, db, session)
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e3", SessionName: session, Time: time.Now().UTC(), Type: "b", Direction: event.Internal}); err != nil {
		t.Fatalf("append e3 to second incarnation: %v", err)
	}

	has, err := db.HasEventCursor(ctx, session, cursorName)
	if err != nil {
		t.Fatalf("has event cursor: %v", err)
	}
	if has {
		t.Fatal("second incarnation reads an existing cursor, want none (its own id has never been committed)")
	}
	pos, err := db.EventCursor(ctx, session, cursorName)
	if err != nil {
		t.Fatalf("event cursor: %v", err)
	}
	if pos != 0 {
		t.Fatalf("cursor position for the new incarnation = %d, want 0 (unread from its own start)", pos)
	}

	evs, _, err := db.ListEventsFrom(ctx, session, pos)
	if err != nil {
		t.Fatalf("list events from %d: %v", pos, err)
	}
	if len(evs) != 1 || evs[0].ID != "e3" {
		t.Fatalf("events from the reported cursor position = %+v, want exactly [e3]", evs)
	}
}

func TestEventCursor_RoundTripAndHasDistinguishesNeverFromZero(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session, cursorName = "s1", "delivery"
	createSessionForTest(t, db, session)

	has, err := db.HasEventCursor(ctx, session, cursorName)
	if err != nil || has {
		t.Fatalf("has before any commit = %v (err=%v), want false", has, err)
	}
	pos, err := db.EventCursor(ctx, session, cursorName)
	if err != nil || pos != 0 {
		t.Fatalf("position before any commit = %d (err=%v), want 0", pos, err)
	}

	if err := db.SetEventCursor(ctx, session, cursorName, 0); err != nil {
		t.Fatalf("set 0: %v", err)
	}
	has, err = db.HasEventCursor(ctx, session, cursorName)
	if err != nil || !has {
		t.Fatalf("has after committing 0 = %v (err=%v), want true", has, err)
	}

	if err := db.SetEventCursor(ctx, session, cursorName, 42); err != nil {
		t.Fatalf("set 42: %v", err)
	}
	pos, err = db.EventCursor(ctx, session, cursorName)
	if err != nil || pos != 42 {
		t.Fatalf("position after set 42 = %d (err=%v), want 42", pos, err)
	}
}

func TestSetEventCursor_RejectsMissingLiveSession(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if err := db.SetEventCursor(ctx, "never/created", "delivery", 0); err == nil {
		t.Fatal("set cursor on a session with no live row succeeded, want error")
	}
}

func TestListEventsFromStreamID_RejectsAStreamBelongingToAnotherSession(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	createSessionForTest(t, db, "victim/session")
	victimStream, err := db.EventStreamID(ctx, "victim/session")
	if err != nil || victimStream == "" {
		t.Fatalf("victim stream id: %q, err=%v", victimStream, err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: "victim/session", Time: time.Now().UTC(), Type: "secret", Source: "test", Direction: event.Internal}); err != nil {
		t.Fatalf("append to victim: %v", err)
	}
	createSessionForTest(t, db, "attacker/session")

	evs, _, err := db.ListEventsFromStreamID(ctx, victimStream, "attacker/session", 0)
	if err == nil {
		t.Fatalf("reading another session's stream by id succeeded, want an ownership error; got events %+v", evs)
	}
}

func TestLatestSessionEventByType_ReturnsMostRecentOfThatType(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	createSessionForTest(t, db, session)

	if _, ok, err := db.LatestSessionEventByType(ctx, session, event.TypeStatusMessage); err != nil || ok {
		t.Fatalf("latest before any event: ok=%v err=%v, want ok=false", ok, err)
	}

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: event.TypeStatusMessage, Direction: event.Outbound, Summary: "first"}); err != nil {
		t.Fatalf("append e1: %v", err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e2", SessionName: session, Time: time.Now().UTC(), Type: "other.type", Direction: event.Internal, Summary: "unrelated"}); err != nil {
		t.Fatalf("append e2: %v", err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e3", SessionName: session, Time: time.Now().UTC(), Type: event.TypeStatusMessage, Direction: event.Outbound, Summary: "second"}); err != nil {
		t.Fatalf("append e3: %v", err)
	}

	got, ok, err := db.LatestSessionEventByType(ctx, session, event.TypeStatusMessage)
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v, want ok=true", ok, err)
	}
	if got.ID != "e3" || got.Summary != "second" {
		t.Fatalf("latest = %+v, want the most recent status_message event (e3)", got)
	}
}
