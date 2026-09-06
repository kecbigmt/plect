package service

import (
	"errors"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

func TestEventPublishListShow(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-7"

	ev, err := EventPublish(nil, store, session, EventPublishParams{
		Type:    event.TypeUserNote,
		Summary: "hello",
		Body:    "world",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if ev.ID == "" || ev.Source != event.SourceCLI {
		t.Fatalf("publish defaults wrong: %+v", ev)
	}

	evs, _, _, err := EventList(nil, store, session, 0, event.Filter{})
	if err != nil || len(evs) != 1 || evs[0].Summary != "hello" {
		t.Fatalf("list: err=%v evs=%+v", err, evs)
	}

	got, err := EventShow(nil, store, session, ev.ID)
	if err != nil || got.ID != ev.ID {
		t.Fatalf("show: err=%v got=%+v", err, got)
	}

	if _, err := EventShow(nil, store, session, "nope"); err == nil {
		t.Fatalf("expected not-found error for unknown id")
	}
}

// TestEventPublishDirectionNormalization covers the direction normalization rule:
// direction is forced to Inbound whenever PLECT_SESSION_NAME names a session
// other than the publish target, and origin_session is stamped so a later
// reader can tell who the event came from. A same-session publish (the
// orchestrator pattern that must not reset its own tick backoff) keeps
// whatever direction the caller asked for.
func TestEventPublishDirectionNormalization(t *testing.T) {
	store := state.NewStore(t.TempDir())

	t.Run("cross-session publish forces inbound", func(t *testing.T) {
		t.Setenv("PLECT_SESSION_NAME", "owner/other")
		ev, err := EventPublish(nil, store, "owner/target", EventPublishParams{
			Type:      event.TypeUserEmit,
			Direction: event.Internal, // caller's request must be overridden
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if ev.Direction != event.Inbound {
			t.Fatalf("direction = %q, want inbound", ev.Direction)
		}
		if ev.Metadata[event.MetaOriginSession] != "owner/other" {
			t.Fatalf("origin_session = %q, want owner/other", ev.Metadata[event.MetaOriginSession])
		}
	})

	t.Run("same-session publish keeps requested direction", func(t *testing.T) {
		t.Setenv("PLECT_SESSION_NAME", "owner/self")
		ev, err := EventPublish(nil, store, "owner/self", EventPublishParams{
			Type:      event.TypeUserEmit,
			Direction: event.Internal,
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if ev.Direction != event.Internal {
			t.Fatalf("direction = %q, want internal (self-publish must not become inbound)", ev.Direction)
		}
		if ev.Metadata[event.MetaOriginSession] != "owner/self" {
			t.Fatalf("origin_session = %q, want owner/self", ev.Metadata[event.MetaOriginSession])
		}
	})

	t.Run("no ambient session leaves direction as requested", func(t *testing.T) {
		t.Setenv("PLECT_SESSION_NAME", "")
		ev, err := EventPublish(nil, store, "owner/target", EventPublishParams{
			Type:      event.TypeUserEmit,
			Direction: event.Outbound,
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
		if ev.Direction != event.Outbound {
			t.Fatalf("direction = %q, want outbound (unchanged)", ev.Direction)
		}
		if _, ok := ev.Metadata[event.MetaOriginSession]; ok {
			t.Fatalf("origin_session stamped with no ambient session: %+v", ev.Metadata)
		}
	})
}

// A guarded orchestrator (PLECT_SESSION_GUARD → cfg.SessionGuard) must not be
// able to publish into a session outside its name space, even though `plect ls`
// reveals the name.
func TestEventPublishSessionGuardBlocksCrossOwner(t *testing.T) {
	store := state.NewStore(t.TempDir())
	cfg := &config.Config{SessionGuard: "^acme/"}

	_, err := EventPublish(cfg, store, "exampleorg/repo-26", EventPublishParams{
		Type: event.TypeUserEmit, Summary: "inject",
	})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrRepoNotAllowed {
		t.Fatalf("want ErrRepoNotAllowed for cross-owner publish, got %v", err)
	}
	// The rejected publish must not have touched the target's log.
	evs, _, _, lerr := EventList(cfg, store, "exampleorg/repo-26", 0, event.Filter{})
	if lerr != nil || len(evs) != 0 {
		t.Fatalf("rejected publish leaked into the log: err=%v evs=%+v", lerr, evs)
	}
}

// The guard permits publishes that match the orchestrator's own name space.
func TestEventPublishSessionGuardAllowsMatching(t *testing.T) {
	store := state.NewStore(t.TempDir())
	cfg := &config.Config{SessionGuard: "^acme/"}

	if _, err := EventPublish(cfg, store, "acme/repo-1", EventPublishParams{
		Type: event.TypeUserNote, Summary: "ok",
	}); err != nil {
		t.Fatalf("guard must allow a matching session: %v", err)
	}
}

// Backward compatibility: with no guard configured (the coding-session default)
// every session remains publishable.
func TestEventPublishNoGuardAllowsCrossOwner(t *testing.T) {
	store := state.NewStore(t.TempDir())
	cfg := &config.Config{} // SessionGuard == ""

	if _, err := EventPublish(cfg, store, "exampleorg/repo-26", EventPublishParams{
		Type: event.TypeUserNote, Summary: "ok",
	}); err != nil {
		t.Fatalf("an unset guard must allow all publishes: %v", err)
	}
}

func TestEventPublishRequiresType(t *testing.T) {
	store := state.NewStore(t.TempDir())
	_, err := EventPublish(nil, store, "o/r-1", EventPublishParams{Summary: "no type"})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestEventPageDescReturnsNewestN(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-7"
	for i := range 5 {
		if _, err := EventPublish(nil, store, session, EventPublishParams{
			Type: event.TypeUserNote, Summary: string(rune('A' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// desc + limit returns the most recent N, newest first, with no cursor.
	page, err := EventPage(nil, store, session, EventPageParams{
		Order:  event.OrderDesc,
		Filter: event.Filter{Limit: 2},
	})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page.Events) != 2 || page.Events[0].Summary != "E" || page.Events[1].Summary != "D" {
		t.Fatalf("desc page = %+v", page.Events)
	}
	if page.NextCursor != "" {
		t.Fatalf("desc must not paginate in v1, got cursor %q", page.NextCursor)
	}
}

func TestEventPageAscPaginatesWithCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-7"
	for i := range 5 {
		if _, err := EventPublish(nil, store, session, EventPublishParams{
			Type: event.TypeUserNote, Summary: string(rune('A' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := EventPage(nil, store, session, EventPageParams{Filter: event.Filter{Limit: 2}})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Events) != 2 || first.Events[0].Summary != "A" || first.NextCursor == "" {
		t.Fatalf("first asc page = %+v cursor=%q", first.Events, first.NextCursor)
	}

	second, err := EventPage(nil, store, session, EventPageParams{
		Cursor: first.NextCursor,
		Filter: event.Filter{Limit: 2},
	})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Events) != 2 || second.Events[0].Summary != "C" {
		t.Fatalf("second asc page = %+v", second.Events)
	}
}

func TestEventPageRejectsOrderMismatchCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-7"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	page, err := EventPage(nil, store, session, EventPageParams{Filter: event.Filter{Limit: 1}})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("setup page: err=%v cursor=%q", err, page.NextCursor)
	}

	// An asc-issued cursor used with --order desc must be rejected, not silently
	// reinterpreted.
	_, err = EventPage(nil, store, session, EventPageParams{Order: event.OrderDesc, Cursor: page.NextCursor})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for order mismatch, got %v", err)
	}
}

func TestEventPageRejectsStaleGenerationCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-7"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	// Forge a cursor with a generation the log never had.
	stale := event.Cursor{V: event.CursorVersion, Off: 0, Ord: event.OrderAsc, StreamID: "01JXNEVER"}.Encode()
	_, err := EventPage(nil, store, session, EventPageParams{Cursor: stale})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for stale generation, got %v", err)
	}
}

func TestEventPageRejectsCursorAcrossSessionDeleteAndRecreateUnderSameName(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-7"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	page, err := EventPage(nil, store, session, EventPageParams{Filter: event.Filter{Limit: 1}})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("setup page: err=%v cursor=%q", err, page.NextCursor)
	}

	if err := store.Delete(session); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := eventlog.NewStore(store.Dir()).NewStream(session); err != nil {
		t.Fatalf("new stream on recreate: %v", err)
	}
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatalf("publish after recreate: %v", err)
	}

	_, err = EventPage(nil, store, session, EventPageParams{Cursor: page.NextCursor})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("cursor from before a real destroy+recreate = %v, want ErrInvalidInput (stale against the new incarnation)", err)
	}
}

// A cursor encoded under the retired byte-offset format must never be
// reinterpreted as a sequence number.
func TestEventPageRejectsOldVersionCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-7"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	old := event.Cursor{V: event.CursorVersion - 1, Off: 0, Ord: event.OrderAsc, StreamID: "01JXNEVER"}.Encode()
	_, err := EventPage(nil, store, session, EventPageParams{Cursor: old})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for an old-version cursor, got %v", err)
	}
}

func TestEventStreamResumeDecodesEventPageCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-9"
	for range 3 {
		if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := EventPage(nil, store, session, EventPageParams{Filter: event.Filter{Limit: 2}})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("setup page: err=%v cursor=%q", err, page.NextCursor)
	}

	gen, offset, err := EventStreamResume(nil, store, session, page.NextCursor)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if gen == "" {
		t.Fatal("resume: expected a non-empty generation")
	}

	rest, err := EventPage(nil, store, session, EventPageParams{Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("rest page: %v", err)
	}
	if len(rest.Events) != 1 {
		t.Fatalf("rest page = %+v, want the one remaining event", rest.Events)
	}
	direct, _, _, err := EventList(nil, store, session, offset, event.Filter{})
	if err != nil || len(direct) != 1 || direct[0].ID != rest.Events[0].ID {
		t.Fatalf("EventList from resumed offset = %+v (err=%v), want %+v", direct, err, rest.Events)
	}
}

func TestEventStreamResumeEmptyCursorIsFreshConnect(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-9"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	gen, offset, err := EventStreamResume(nil, store, session, "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if gen == "" {
		t.Fatal("resume: expected a non-empty generation even with no cursor")
	}
	if offset != 0 {
		t.Fatalf("offset = %d, want 0 for a fresh connect", offset)
	}
}

func TestEventStreamResumeRejectsStaleGenerationCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-9"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	stale := event.Cursor{V: event.CursorVersion, Off: 0, Ord: event.OrderAsc, StreamID: "01JXNEVER"}.Encode()
	_, _, err := EventStreamResume(nil, store, session, stale)
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for stale generation, got %v", err)
	}
}

func TestEventStreamResumeRejectsOldVersionCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-9"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	old := event.Cursor{V: event.CursorVersion - 1, Off: 0, Ord: event.OrderAsc, StreamID: "01JXNEVER"}.Encode()
	_, _, err := EventStreamResume(nil, store, session, old)
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for an old-version cursor, got %v", err)
	}
}

func TestEventStreamResumeRejectsOrderMismatchCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-9"
	if _, err := EventPublish(nil, store, session, EventPublishParams{Type: event.TypeUserNote}); err != nil {
		t.Fatal(err)
	}
	page, err := EventPage(nil, store, session, EventPageParams{Filter: event.Filter{Limit: 1}})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("setup page: err=%v cursor=%q", err, page.NextCursor)
	}
	cur, err := event.DecodeCursor(page.NextCursor)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cur.Ord = event.OrderDesc
	_, _, err = EventStreamResume(nil, store, session, cur.Encode())
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for order mismatch, got %v", err)
	}
}

func TestEventStreamResumeRejectsMalformedCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	const session = "owner/repo-9"
	_, _, err := EventStreamResume(nil, store, session, "not-a-real-cursor")
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for malformed cursor, got %v", err)
	}
}

func TestEventListUnknownSessionIsEmpty(t *testing.T) {
	store := state.NewStore(t.TempDir())
	evs, _, next, err := EventList(nil, store, "never/created-1", 0, event.Filter{})
	if err != nil {
		t.Fatalf("list of unknown session should not error: %v", err)
	}
	if len(evs) != 0 || next != 0 {
		t.Fatalf("expected empty, got evs=%d next=%d", len(evs), next)
	}
}

func TestEventResolvesAlias(t *testing.T) {
	store := state.NewStore(t.TempDir())
	if err := store.Put(&domain.Session{Name: "owner/repo-7", Alias: "my-feature"}); err != nil {
		t.Fatal(err)
	}
	// publish by the canonical session name…
	if _, err := EventPublish(nil, store, "owner/repo-7", EventPublishParams{Type: event.TypeUserNote, Summary: "via name"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// …and read it back by the session's alias (resolver/alias support).
	evs, _, _, err := EventList(nil, store, "my-feature", 0, event.Filter{})
	if err != nil {
		t.Fatalf("list by alias: %v", err)
	}
	if len(evs) != 1 || evs[0].Summary != "via name" {
		t.Fatalf("alias did not resolve to the session: got %+v", evs)
	}
}

func summaryList(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i := range evs {
		out[i] = evs[i].Summary
	}
	return out
}

// EventPageSubtree merges a session tree (root + descendants) in time order,
// scoping by the canonical tree rather than a stream id, and pages by ULID
// keyset like the stream view (ADR §7, slice 2).
func TestEventPageSubtreePaginatesAcrossTree(t *testing.T) {
	store := state.NewStore(t.TempDir())
	// root -> work -> grandchild, plus an unrelated session that must not bleed in.
	tree := []*domain.Session{
		{Name: "root"},
		{Name: "work", ParentSession: "root"},
		{Name: "grandchild", ParentSession: "work"},
		{Name: "outside"},
	}
	for _, s := range tree {
		if err := store.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	// Interleave appends across the subtree so the merged order spans sessions.
	plan := []struct{ session, summary string }{
		{"root", "A"}, {"work", "B"}, {"grandchild", "C"}, {"work", "D"}, {"root", "E"},
	}
	for _, p := range plan {
		if _, err := EventPublish(nil, store, p.session, EventPublishParams{Type: event.TypeUserNote, Summary: p.summary}); err != nil {
			t.Fatal(err)
		}
	}
	// Event in a session outside the subtree must never appear.
	if _, err := EventPublish(nil, store, "outside", EventPublishParams{Type: event.TypeUserNote, Summary: "X"}); err != nil {
		t.Fatal(err)
	}

	first, err := EventPageSubtree(nil, store, "root", EventPageParams{Filter: event.Filter{Limit: 2}})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Events) != 2 || first.Events[0].Summary != "A" || first.Events[1].Summary != "B" || first.NextCursor == "" {
		t.Fatalf("first page = %+v cursor=%q", summaryList(first.Events), first.NextCursor)
	}

	second, err := EventPageSubtree(nil, store, "root", EventPageParams{Cursor: first.NextCursor, Filter: event.Filter{Limit: 2}})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Events) != 2 || second.Events[0].Summary != "C" || second.Events[1].Summary != "D" {
		t.Fatalf("second page = %+v", summaryList(second.Events))
	}

	third, err := EventPageSubtree(nil, store, "root", EventPageParams{Cursor: second.NextCursor, Filter: event.Filter{Limit: 2}})
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	if len(third.Events) != 1 || third.Events[0].Summary != "E" {
		t.Fatalf("third page = %+v", summaryList(third.Events))
	}

	caughtUp, err := EventPageSubtree(nil, store, "root", EventPageParams{Cursor: third.NextCursor, Filter: event.Filter{Limit: 2}})
	if err != nil {
		t.Fatalf("caught-up page: %v", err)
	}
	if len(caughtUp.Events) != 0 || caughtUp.NextCursor != "" {
		t.Fatalf("caught-up page = %+v cursor=%q, want empty", summaryList(caughtUp.Events), caughtUp.NextCursor)
	}
}

// A subtree query scoped at an interior node returns that node's events plus its
// descendants', but not its parent's or siblings' — the visibility basis for a
// parent observing only its own subtree (ADR §7).
func TestEventPageSubtreeScopesToInteriorNode(t *testing.T) {
	store := state.NewStore(t.TempDir())
	tree := []*domain.Session{
		{Name: "root"},
		{Name: "work", ParentSession: "root"},
		{Name: "review", ParentSession: "root"},
		{Name: "child", ParentSession: "work"},
	}
	for _, s := range tree {
		if err := store.Put(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []struct{ session, summary string }{
		{"root", "root-ev"}, {"work", "work-ev"}, {"child", "child-ev"}, {"review", "review-ev"},
	} {
		if _, err := EventPublish(nil, store, p.session, EventPublishParams{Type: event.TypeUserNote, Summary: p.summary}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := EventPageSubtree(nil, store, "work", EventPageParams{})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	got := summaryList(page.Events)
	want := []string{"work-ev", "child-ev"}
	if len(got) != len(want) {
		t.Fatalf("subtree(work) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subtree(work) = %v, want %v", got, want)
		}
	}
}

func TestEventPageSubtreeRejectsCrossRootCursor(t *testing.T) {
	store := state.NewStore(t.TempDir())
	for _, n := range []string{"root-a", "root-b"} {
		if err := store.Put(&domain.Session{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := EventPublish(nil, store, "root-a", EventPublishParams{Type: event.TypeUserNote, Summary: "A"}); err != nil {
		t.Fatal(err)
	}
	page, err := EventPageSubtree(nil, store, "root-a", EventPageParams{Filter: event.Filter{Limit: 1}})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if page.NextCursor == "" {
		t.Fatal("want a resume cursor")
	}
	_, err = EventPageSubtree(nil, store, "root-b", EventPageParams{Cursor: page.NextCursor})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrInvalidInput {
		t.Fatalf("want ErrInvalidInput for cross-root cursor, got %v", err)
	}
}

// A subtree root absent from state has no tree, so the query is an explicit
// error rather than silently falling back to a single-session log read.
func TestEventPageSubtreeUnknownRootErrors(t *testing.T) {
	store := state.NewStore(t.TempDir())
	_, err := EventPageSubtree(nil, store, "ghost/repo-1", EventPageParams{})
	var svcErr *Error
	if !errors.As(err, &svcErr) || svcErr.Code != ErrSessionNotFound {
		t.Fatalf("want ErrSessionNotFound for unknown root, got %v", err)
	}
}
