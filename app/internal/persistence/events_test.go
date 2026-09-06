package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/contracts/event"
)

func TestAppendEvent_AssignsIncreasingPerStreamSequence(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	if _, err := db.CreateEventStream(ctx, session); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	if _, err := db.CreateEventStream(ctx, "s2"); err != nil {
		t.Fatalf("create stream s2: %v", err)
	}

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

	// A different session's stream starts its own sequence at 1.
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
	if _, err := db.CreateEventStream(ctx, session); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	want := event.Event{
		ID:           "01JXAMPLE",
		SessionName:  session,
		Time:         when,
		Type:         "widget.message",
		Source:       "widget",
		Direction:    event.Inbound,
		Summary:      "hello",
		Body:         "hello body",
		Metadata:     map[string]string{"k": "v"},
		DeliveryMode: event.DeliveryModePush,
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
		ev.Summary != want.Summary || ev.Body != want.Body || ev.DeliveryMode != want.DeliveryMode {
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
	if _, err := db.CreateEventStream(ctx, session); err != nil {
		t.Fatalf("create stream: %v", err)
	}

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
	if _, err := db.CreateEventStream(ctx, session); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a"}); err == nil {
		t.Fatal("append with empty direction succeeded, want error")
	}
}

func TestAppendEvent_RejectsMissingCurrentStream(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: "never/created", Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err == nil {
		t.Fatal("append to a session with no current stream succeeded, want error")
	}
}

func TestListEventsFrom_ResumesAfterAGivenSequence(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"
	if _, err := db.CreateEventStream(ctx, session); err != nil {
		t.Fatalf("create stream: %v", err)
	}

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

func TestListEventsFrom_MissingStreamIsEmptyNotError(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	evs, seqs, err := db.ListEventsFrom(ctx, "never/created", 0)
	if err != nil || len(evs) != 0 || len(seqs) != 0 {
		t.Fatalf("list on missing stream = (%v, %v, %v), want empty/no error", evs, seqs, err)
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

	created, err := db.CreateEventStream(ctx, session)
	if err != nil || created == "" {
		t.Fatalf("create stream: id=%q err=%v", created, err)
	}
	g1, err := db.EventStreamID(ctx, session)
	if err != nil || g1 != created {
		t.Fatalf("stream id after create = %q (err=%v), want %q", g1, err, created)
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

// TestEventStreamID_RecreateMintsANewCurrentIncarnation pins that
// CreateEventStream always inserts, never looks up first, so calling it
// again for the same session name (a session create on a name whose prior
// incarnation was destroyed) mints a distinct id and EventStreamID resolves
// to that new row — the latest by created_at — not the superseded one. The
// old stream's own rows are untouched (no delete anywhere in this package),
// so its sequence does not leak into the new stream's numbering.
func TestEventStreamID_RecreateMintsANewCurrentIncarnation(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"

	first, err := db.CreateEventStream(ctx, session)
	if err != nil {
		t.Fatalf("create stream (first): %v", err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err != nil {
		t.Fatalf("append to first incarnation: %v", err)
	}

	second, err := db.CreateEventStream(ctx, session)
	if err != nil {
		t.Fatalf("create stream (second): %v", err)
	}
	if second == first {
		t.Fatalf("second incarnation's stream id = %q, want distinct from first %q", second, first)
	}

	current, err := db.EventStreamID(ctx, session)
	if err != nil || current != second {
		t.Fatalf("current stream id = %q (err=%v), want the second incarnation %q", current, err, second)
	}

	seq, err := db.AppendEvent(ctx, event.Event{ID: "e2", SessionName: session, Time: time.Now().UTC(), Type: "b", Direction: event.Internal})
	if err != nil {
		t.Fatalf("append to second incarnation: %v", err)
	}
	if seq != 1 {
		t.Fatalf("second incarnation's first sequence = %d, want 1 (its own stream, not continuing the first's)", seq)
	}
}

func TestEventCursor_RoundTripAndHasDistinguishesNeverFromZero(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session, cursorName = "s1", "delivery"
	if _, err := db.CreateEventStream(ctx, session); err != nil {
		t.Fatalf("create stream: %v", err)
	}

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

func TestSetEventCursor_RejectsMissingCurrentStream(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	if err := db.SetEventCursor(ctx, "never/created", "delivery", 0); err == nil {
		t.Fatal("set cursor on a session with no current stream succeeded, want error")
	}
}

func TestListEventsFromStreamID_RejectsAStreamBelongingToAnotherSession(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()

	victimStream, err := db.CreateEventStream(ctx, "victim/session")
	if err != nil {
		t.Fatalf("create victim stream: %v", err)
	}
	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: "victim/session", Time: time.Now().UTC(), Type: "secret", Source: "test", Direction: event.Internal}); err != nil {
		t.Fatalf("append to victim: %v", err)
	}
	if _, err := db.CreateEventStream(ctx, "attacker/session"); err != nil {
		t.Fatalf("create attacker stream: %v", err)
	}

	evs, _, err := db.ListEventsFromStreamID(ctx, victimStream, "attacker/session", 0)
	if err == nil {
		t.Fatalf("reading another session's stream by id succeeded, want an ownership error; got events %+v", evs)
	}
}
