package event

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}, srv
}

func TestClientPublish(t *testing.T) {
	var gotBody Event
	var gotAuth string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/events" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "01EVID", "offset": 42})
	})
	client.Token = "secret"

	ev := Event{SessionName: "workspace-1", Type: "acme.ci_status", Source: "example"}
	id, off, err := client.Publish(context.Background(), ev)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if id != "01EVID" || off != 42 {
		t.Fatalf("Publish = (%q, %d), want (01EVID, 42)", id, off)
	}
	if gotBody.SessionName != ev.SessionName || gotBody.Type != ev.Type {
		t.Fatalf("server received %+v, want session/type from %+v", gotBody, ev)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer secret")
	}
}

func TestClientPublishNoTokenOmitsAuthHeader(t *testing.T) {
	var gotAuth string
	seen := false
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		seen = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "offset": 0})
	})

	if _, _, err := client.Publish(context.Background(), Event{SessionName: "s"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !seen {
		t.Fatalf("server did not receive request")
	}
	if gotAuth != "" {
		t.Fatalf("Authorization header = %q, want empty", gotAuth)
	}
}

func TestClientPublishErrorStatus(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, _, err := client.Publish(context.Background(), Event{SessionName: "s"}); err == nil {
		t.Fatalf("expected error on non-200 status")
	}
}

func TestClientList(t *testing.T) {
	var gotQuery url.Values
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listResponse{
			Events:     []Event{{ID: "1", SessionName: "workspace-1"}},
			NextCursor: "cursortoken",
		})
	})

	evs, next, err := client.List(context.Background(), "workspace-1", OrderDesc, "prevcursor", Filter{
		Types:   []string{"acme.*"},
		Sources: []string{"example"},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evs) != 1 || evs[0].ID != "1" {
		t.Fatalf("List events = %+v, want one event with ID 1", evs)
	}
	if next != "cursortoken" {
		t.Fatalf("List next cursor = %q, want %q", next, "cursortoken")
	}

	if gotQuery.Get("session") != "workspace-1" {
		t.Errorf("session query = %q, want workspace-1", gotQuery.Get("session"))
	}
	if gotQuery.Get("order") != "desc" {
		t.Errorf("order query = %q, want desc", gotQuery.Get("order"))
	}
	if gotQuery.Get("cursor") != "prevcursor" {
		t.Errorf("cursor query = %q, want prevcursor", gotQuery.Get("cursor"))
	}
	if gotQuery.Get("types") != "acme.*" {
		t.Errorf("types query = %q, want acme.*", gotQuery.Get("types"))
	}
	if gotQuery.Get("source") != "example" {
		t.Errorf("source query = %q, want %q", gotQuery.Get("source"), "example")
	}
	if gotQuery.Get("limit") != "10" {
		t.Errorf("limit query = %q, want 10", gotQuery.Get("limit"))
	}
}

func TestClientListDefaultOrderOmitted(t *testing.T) {
	var gotQuery url.Values
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listResponse{})
	})

	if _, _, err := client.List(context.Background(), "s", OrderAsc, "", Filter{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if gotQuery.Has("order") {
		t.Errorf("expected order query param omitted for default asc order, got %q", gotQuery.Get("order"))
	}
	if gotQuery.Has("cursor") {
		t.Errorf("expected cursor query param omitted for empty cursor")
	}
}

func TestClientListErrorStatus(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	if _, _, err := client.List(context.Background(), "s", OrderAsc, "", Filter{}); err == nil {
		t.Fatalf("expected error on non-200 status")
	}
}

func TestClientSubscribeReplaysAndFollowsSSE(t *testing.T) {
	var gotLastEventID string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/stream" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		gotLastEventID = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		ev1, _ := json.Marshal(Event{ID: "1", SessionName: "s", Type: "a"})
		ev2, _ := json.Marshal(Event{ID: "2", SessionName: "s", Type: "b"})
		fmt.Fprintf(w, "id: streamA:10\ndata: %s\n\n", ev1)
		fmt.Fprintf(w, "id: streamA:20\ndata: %s\n\n", ev2)
	})

	var mu sync.Mutex
	var got []Event
	var offs []int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.Subscribe(ctx, "s", 5, Filter{}, func(ev Event, off int64) {
			mu.Lock()
			got = append(got, ev)
			offs = append(offs, off)
			mu.Unlock()
			if len(got) == 2 {
				cancel()
			}
		})
	}()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Subscribe returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return in time")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("received events %+v, want ids 1, 2", got)
	}
	if offs[0] != 10 || offs[1] != 20 {
		t.Fatalf("received offsets %v, want [10 20]", offs)
	}
	if gotLastEventID != ":5" {
		t.Fatalf("Last-Event-ID header = %q, want %q", gotLastEventID, ":5")
	}
}

func TestClientSubscribeReconnectsOnStreamEnd(t *testing.T) {
	var mu sync.Mutex
	var lastEventIDs []string

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastEventIDs = append(lastEventIDs, r.Header.Get("Last-Event-ID"))
		attempt := len(lastEventIDs)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if attempt == 1 {
			ev, _ := json.Marshal(Event{ID: "1", SessionName: "s"})
			fmt.Fprintf(w, "id: streamA:100\ndata: %s\n\n", ev)
			return // stream ends; client must reconnect with Last-Event-ID: streamA:100
		}
		ev, _ := json.Marshal(Event{ID: "2", SessionName: "s"})
		fmt.Fprintf(w, "id: streamA:200\ndata: %s\n\n", ev)
	})

	var got []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- client.Subscribe(ctx, "s", 0, Filter{}, func(ev Event, off int64) {
			mu.Lock()
			got = append(got, ev.ID)
			mu.Unlock()
			if len(got) == 2 {
				cancel()
			}
		})
	}()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Subscribe returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not reconnect and deliver both events in time")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("received event ids %v, want [1 2]", got)
	}
	if len(lastEventIDs) < 2 {
		t.Fatalf("expected at least 2 connection attempts, got %d", len(lastEventIDs))
	}
	if lastEventIDs[1] != "streamA:100" {
		t.Fatalf("reconnect Last-Event-ID = %q, want %q (resume from prior stream+offset)", lastEventIDs[1], "streamA:100")
	}
}

func TestClientSubscribeErrorStatusTriggersRetry(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := client.Subscribe(ctx, "s", 0, Filter{}, func(Event, int64) {})
	if err != context.DeadlineExceeded {
		t.Fatalf("Subscribe returned %v, want context.DeadlineExceeded", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Fatalf("expected retry after error status, got %d attempt(s)", attempts)
	}
}

func TestFilterQueryAndListQueryEncoding(t *testing.T) {
	f := Filter{
		Types:     []string{"acme.*", "widget.*"},
		Sources:   []string{"example", "other"},
		Direction: Inbound,
		Limit:     5,
	}

	lq := listQuery("workspace-1", OrderDesc, "abc", f)
	if lq.Get("session") != "workspace-1" {
		t.Errorf("listQuery session = %q", lq.Get("session"))
	}
	if lq.Get("order") != "desc" {
		t.Errorf("listQuery order = %q", lq.Get("order"))
	}
	if lq.Get("cursor") != "abc" {
		t.Errorf("listQuery cursor = %q", lq.Get("cursor"))
	}
	if lq.Get("direction") != string(Inbound) {
		t.Errorf("listQuery direction = %q", lq.Get("direction"))
	}
	if !strings.Contains(lq.Get("types"), "acme.*") {
		t.Errorf("listQuery types = %q", lq.Get("types"))
	}

	sq := filterQuery("workspace-1", "streamA:42", f)
	if sq.Get("session") != "workspace-1" {
		t.Errorf("filterQuery session = %q", sq.Get("session"))
	}
	if sq.Get("since") != "streamA:42" {
		t.Errorf("filterQuery since = %q, want streamA:42", sq.Get("since"))
	}

	// an empty resume token is omitted: an unset resume position, not offset zero.
	sq0 := filterQuery("workspace-1", "", Filter{})
	if sq0.Has("since") {
		t.Errorf("filterQuery since should be omitted for an empty resume token, got %q", sq0.Get("since"))
	}
}
