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

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: "s1", Time: time.Now().UTC(), Type: "a"}); err == nil {
		t.Fatal("append with empty direction succeeded, want error")
	}
}

func TestAppendEvent_DeliveryModeIsNotPersisted(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"

	if _, err := db.AppendEvent(ctx, event.Event{
		ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a",
		Direction: event.Internal, DeliveryMode: event.DeliveryModePush,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _, err := db.ListEventsFrom(ctx, session, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].DeliveryMode != "" {
		t.Fatalf("delivery mode = %q, want zero value on read", got[0].DeliveryMode)
	}
}

func TestListEventsFrom_ResumesAfterAGivenSequence(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s1"

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

func TestEventStreamID_EmptyUntilFirstTouchThenStable(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session = "s9"

	gen, err := db.EventStreamID(ctx, session)
	if err != nil || gen != "" {
		t.Fatalf("gen before any touch = %q (err=%v), want empty", gen, err)
	}

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e1", SessionName: session, Time: time.Now().UTC(), Type: "a", Direction: event.Internal}); err != nil {
		t.Fatalf("append: %v", err)
	}
	g1, err := db.EventStreamID(ctx, session)
	if err != nil || g1 == "" {
		t.Fatalf("gen after first append = %q (err=%v), want non-empty", g1, err)
	}

	if _, err := db.AppendEvent(ctx, event.Event{ID: "e2", SessionName: session, Time: time.Now().UTC(), Type: "b", Direction: event.Internal}); err != nil {
		t.Fatalf("append: %v", err)
	}
	g2, err := db.EventStreamID(ctx, session)
	if err != nil || g2 != g1 {
		t.Fatalf("gen changed across appends: %q -> %q", g1, g2)
	}
}

func TestEventCursor_RoundTripAndHasDistinguishesNeverFromZero(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session, cursorName = "s1", "delivery"

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

func TestSetEventCursor_CreatesStreamWhenNoEventExistsYet(t *testing.T) {
	db := migratedTestDB(t)
	ctx := context.Background()
	const session, cursorName = "s1", "delivery"

	if err := db.SetEventCursor(ctx, session, cursorName, 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	gen, err := db.EventStreamID(ctx, session)
	if err != nil || gen == "" {
		t.Fatalf("gen after seeding a cursor with no events = %q (err=%v), want non-empty", gen, err)
	}
}
