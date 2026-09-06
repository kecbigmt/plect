package eventbus

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/contracts/event"
)

func newTestBus(t *testing.T, token string) (*event.Client, string, *eventlog.Store) {
	t.Helper()
	store := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(store)
	t.Cleanup(hub.Close)
	ts := httptest.NewServer(New(store, token, hub).Routes())
	t.Cleanup(ts.Close)
	return &event.Client{BaseURL: ts.URL, Token: token, HTTP: ts.Client()}, ts.URL, store
}

func TestBus_PublishAndList(t *testing.T) {
	c, _, _ := newTestBus(t, "")
	ctx := t.Context()

	id, off, err := c.Publish(ctx, event.Event{SessionName: "owner/repo-1", Type: "user.note", Summary: "hi", Direction: event.Internal})
	if err != nil || id == "" || off != 1 {
		t.Fatalf("publish: id=%q off=%d err=%v", id, off, err)
	}

	evs, nextCursor, err := c.List(ctx, "owner/repo-1", event.OrderAsc, "", event.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 || evs[0].Summary != "hi" || evs[0].ID != id {
		t.Fatalf("list events = %+v", evs)
	}
	if nextCursor == "" {
		t.Fatal("next_cursor should be non-empty once the log exists")
	}
	// paging from the tail cursor yields nothing new (no re-delivery).
	more, _, err := c.List(ctx, "owner/repo-1", event.OrderAsc, nextCursor, event.Filter{})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(more) != 0 {
		t.Fatalf("page 2 = %+v, want empty", more)
	}
}

func TestBus_ListFilter(t *testing.T) {
	c, _, _ := newTestBus(t, "")
	ctx := t.Context()
	_, _, _ = c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "github.ci_status", Source: "github", Direction: event.Internal})
	_, _, _ = c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "slack.message", Source: "slack", Direction: event.Internal})

	gh, _, err := c.List(ctx, "o/r-1", event.OrderAsc, "", event.Filter{Types: []string{"github.*"}})
	if err != nil || len(gh) != 1 || gh[0].Type != "github.ci_status" {
		t.Fatalf("github.* filter: %v len=%d", err, len(gh))
	}

	// desc returns newest first.
	desc, _, err := c.List(ctx, "o/r-1", event.OrderDesc, "", event.Filter{})
	if err != nil || len(desc) != 2 || desc[0].Type != "slack.message" {
		t.Fatalf("desc order: %v %+v", err, desc)
	}
}

func TestBus_AuthRequired(t *testing.T) {
	_, baseURL, _ := newTestBus(t, "s3cret")

	noTok := &event.Client{BaseURL: baseURL, HTTP: http.DefaultClient}
	if _, _, err := noTok.Publish(t.Context(), event.Event{SessionName: "o/r-1", Type: "user.note", Direction: event.Internal}); err == nil {
		t.Fatal("publish without token should be rejected")
	}

	withTok := &event.Client{BaseURL: baseURL, Token: "s3cret", HTTP: http.DefaultClient}
	if _, _, err := withTok.Publish(t.Context(), event.Event{SessionName: "o/r-1", Type: "user.note", Direction: event.Internal}); err != nil {
		t.Fatalf("publish with token should succeed: %v", err)
	}
}

func TestBus_StreamReplayThenLive(t *testing.T) {
	c, _, _ := newTestBus(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// pre-existing event must be replayed.
	if _, _, err := c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: "first", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}

	got := make(chan event.Event, 8)
	go func() {
		_ = c.Subscribe(ctx, "o/r-1", 0, event.Filter{}, func(ev event.Event, _ int64) { got <- ev })
	}()

	if ev := recv(t, got); ev.Summary != "first" {
		t.Fatalf("replay = %q, want first", ev.Summary)
	}

	// a subsequent append must arrive live.
	if _, _, err := c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: "second", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}
	if ev := recv(t, got); ev.Summary != "second" {
		t.Fatalf("live = %q, want second", ev.Summary)
	}
}

// TestBus_StreamLiveBurstNoGapNoDup pushes a burst of live appends through the
// shared per-session reader and asserts the subscriber sees each exactly once.
func TestBus_StreamLiveBurstNoGapNoDup(t *testing.T) {
	c, _, _ := newTestBus(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	got := make(chan event.Event, 64)
	go func() {
		_ = c.Subscribe(ctx, "o/r-1", 0, event.Filter{}, func(ev event.Event, _ int64) { got <- ev })
	}()
	time.Sleep(50 * time.Millisecond) // let the subscription connect so appends are live

	const n = 30
	for i := range n {
		if _, _, err := c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: fmt.Sprintf("e%d", i), Direction: event.Internal}); err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool, n)
	deadline := time.After(5 * time.Second)
	for len(seen) < n {
		select {
		case ev := <-got:
			if seen[ev.Summary] {
				t.Fatalf("duplicate event %q", ev.Summary)
			}
			seen[ev.Summary] = true
		case <-deadline:
			t.Fatalf("only %d/%d events delivered", len(seen), n)
		}
	}
}

// TestBus_StreamSurvivesRotation exercises the full stack a browser or CLI
// subscriber actually uses (contracts/event.Client over the bus's live SSE
// endpoint, backed by the shared sessionhub reader) across a same-name
// destroy + recreate: every event must still arrive exactly once, in order,
// including the superseded stream's own unbroadcast tail.
func TestBus_StreamSurvivesRotation(t *testing.T) {
	c, _, store := newTestBus(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	if _, _, err := c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: "before", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}

	got := make(chan event.Event, 8)
	go func() {
		_ = c.Subscribe(ctx, "o/r-1", 0, event.Filter{}, func(ev event.Event, _ int64) { got <- ev })
	}()
	if ev := recv(t, got); ev.Summary != "before" {
		t.Fatalf("replay = %q, want before", ev.Summary)
	}

	if _, err := store.NewStream("o/r-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: "after-1", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: "after-2", Direction: event.Internal}); err != nil {
		t.Fatal(err)
	}

	if ev := recv(t, got); ev.Summary != "after-1" {
		t.Fatalf("post-rotation live event = %q, want after-1 (not dropped, not reordered)", ev.Summary)
	}
	if ev := recv(t, got); ev.Summary != "after-2" {
		t.Fatalf("post-rotation live event = %q, want after-2", ev.Summary)
	}
}

// TestBus_StreamResume locks the cursor contract: each frame's `id` is the
// resume point, and reconnecting with Last-Event-ID delivers events strictly
// after the last one received — no re-delivery, no gap.
func TestBus_StreamResume(t *testing.T) {
	c, baseURL, _ := newTestBus(t, "")
	ctx := t.Context()
	_, _, _ = c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: "A", Direction: event.Internal})
	_, _, _ = c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: "B", Direction: event.Internal})

	id1, ev1 := firstFrame(t, baseURL, "o/r-1", "")
	if ev1.Summary != "A" {
		t.Fatalf("first frame = %q, want A", ev1.Summary)
	}
	_, ev2 := firstFrame(t, baseURL, "o/r-1", id1)
	if ev2.Summary != "B" {
		t.Fatalf("after resume from id %q got %q, want B (no re-delivery of A)", id1, ev2.Summary)
	}
}

// An idle stream (no events) periodically emits a keepalive comment so the
// connection is not torn down for inactivity.
func TestBus_StreamKeepAlive(t *testing.T) {
	orig := keepAliveInterval
	keepAliveInterval = 50 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = orig })

	_, baseURL, _ := newTestBus(t, "")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/stream?session="+url.QueryEscape("o/r-idle"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		// Skip the initial ": connected" comment; any later comment is a keepalive.
		if line := sc.Text(); strings.HasPrefix(line, ": ping") {
			return
		}
	}
	t.Fatal("no keepalive ping on an idle stream")
}

// A fresh stream with ?tail=N replays only the most recent N records, not the
// whole (unbounded) log.
func TestBus_StreamTail(t *testing.T) {
	c, baseURL, _ := newTestBus(t, "")
	ctx := t.Context()
	for _, s := range []string{"A", "B", "C"} {
		if _, _, err := c.Publish(ctx, event.Event{SessionName: "o/r-1", Type: "user.note", Summary: s, Direction: event.Internal}); err != nil {
			t.Fatal(err)
		}
	}

	sctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(sctx, http.MethodGet, baseURL+"/v1/stream?session="+url.QueryEscape("o/r-1")+"&tail=2", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	// Collect the replayed summaries (the stream then idles; cancel via timeout).
	var got []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev event.Event
		if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &ev) == nil {
			got = append(got, ev.Summary)
			if len(got) == 2 { // tail=2 should replay exactly B, C
				break
			}
		}
	}
	if len(got) != 2 || got[0] != "B" || got[1] != "C" {
		t.Fatalf("tail=2 replayed %v, want [B C] (A excluded)", got)
	}
}

func recv(t *testing.T, ch <-chan event.Event) event.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for event")
		return event.Event{}
	}
}

// firstFrame opens an SSE stream and returns the first frame's id + event, then
// closes. lastEventID, if set, is sent as the resume header.
func firstFrame(t *testing.T, baseURL, session, lastEventID string) (string, event.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/stream?session="+url.QueryEscape(session), nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var id, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(line[5:])
		case line == "" && data != "":
			var ev event.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			return id, ev
		}
	}
	t.Fatal("no frame received")
	return "", event.Event{}
}
