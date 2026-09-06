package webui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/contracts/event"
)

// withBus builds a webui Server whose live-timeline handler talks to the given
// (httptest) bus base URL instead of a Unix socket.
func withBus(svc SessionService, busURL string) *Server {
	s := New(svc)
	s.busClientFn = func() *event.Client {
		return &event.Client{BaseURL: busURL, HTTP: http.DefaultClient}
	}
	return s
}

func TestWriteEventFrame(t *testing.T) {
	var sb strings.Builder
	if err := writeEventFrame(&sb, "42", "<li>\n  hi\n</li>"); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	want := "id: 42\ndata: <li>\ndata:   hi\ndata: </li>\n\n"
	if got != want {
		t.Errorf("frame = %q, want %q", got, want)
	}

	// Empty/whitespace HTML emits nothing (a render failure is skipped silently).
	var empty strings.Builder
	if err := writeEventFrame(&empty, "1", "   "); err != nil {
		t.Fatal(err)
	}
	if empty.String() != "" {
		t.Errorf("blank frame = %q, want empty", empty.String())
	}
}

func TestEventsStream_RequiresSession(t *testing.T) {
	rr := get(t, &fakeService{}, "/events/stream")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// fakeBus serves the only bus surface the proxy uses: GET /v1/stream. The stream
// emits one keepalive comment and one multi-line event frame, then ends.
func fakeBus(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// keepalive comment, then one event with a body that spans lines.
		_, _ = w.Write([]byte(": ping\n\n"))
		_, _ = w.Write([]byte("id: 128\ndata: {\"id\":\"E1\",\"session_name\":\"o/r-1\",\"type\":\"claude.reply\",\"source\":\"claude\",\"summary\":\"hi there\"}\n\n"))
		w.(http.Flusher).Flush()
	})
	return httptest.NewServer(mux)
}

// The proxy relays the bus stream to the browser as rendered <li> rows, carrying
// the resume id and forwarding keepalives.
func TestEventsStream_RelaysRenderedRows(t *testing.T) {
	bus := fakeBus(t)
	defer bus.Close()

	srv := httptest.NewServer(withBus(&fakeService{}, bus.URL).Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/stream?session=o/r-1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// Read frames until we see the rendered event row (the fake bus stream ends
	// after it, so the body closes and the scanner stops).
	var sawPing, sawRow bool
	var gotID string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ": ping"):
			sawPing = true
		case strings.HasPrefix(line, "id: "):
			gotID = strings.TrimPrefix(line, "id: ")
		case strings.Contains(line, "claude.reply") && strings.Contains(line, "<li"):
			sawRow = true
		case strings.Contains(line, "hi there"):
			sawRow = true
		}
	}
	if !sawPing {
		t.Error("keepalive comment was not forwarded to the browser")
	}
	// The bus's raw "id: 128" is re-encoded as an opaque v2 cursor, never a bare integer.
	cur, err := event.DecodeCursor(gotID)
	if err != nil {
		t.Fatalf("resume id %q did not decode as an opaque cursor: %v", gotID, err)
	}
	if cur.V != event.CursorVersion || cur.Off != 128 || cur.Ord != event.OrderAsc {
		t.Errorf("resume cursor = %+v, want V=%d Off=128 Ord=asc", cur, event.CursorVersion)
	}
	if !sawRow {
		t.Error("event was not rendered as a timeline row")
	}
}

// A pre-cutover raw byte offset (or any other undecodable token) must be
// rejected before the bus is ever dialed, matching the JSON relay and the
// bus stream's own contract: the client's recovery path is to discard the
// cursor and refetch history, not to receive an unannounced fresh connect.
func TestEventsStream_RawIntegerLastEventIDIsRejectedBeforeDialingBus(t *testing.T) {
	bus := fakeBus(t)
	defer bus.Close()

	var mu sync.Mutex
	var gotCursors []string
	svc := &fakeService{
		// Stands in for service.EventStreamResume: rejects anything that
		// isn't a decodable cursor, exactly as event.DecodeCursor would.
		resumeFn: func(_, cursor string) (string, int64, error) {
			mu.Lock()
			gotCursors = append(gotCursors, cursor)
			mu.Unlock()
			if cursor == "" {
				return "gen1", 0, nil
			}
			if _, err := event.DecodeCursor(cursor); err != nil {
				return "", 0, err
			}
			return "gen1", 0, nil
		},
	}
	srv := httptest.NewServer(withBus(svc, bus.URL).Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events/stream?session=o/r-1", nil)
	req.Header.Set("Last-Event-ID", "128") // pre-cutover raw byte offset
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (rejected before the bus is dialed)", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(gotCursors, "128") {
		t.Fatalf("gotCursors = %v, want the raw Last-Event-ID (128) passed through for validation", gotCursors)
	}
}

// When the bus is unreachable the proxy returns a 502 before committing the
// stream's 200, so the browser's EventSource onerror can surface the degraded
// state rather than stalling on a silent, never-filling stream.
func TestEventsStream_BusUnavailable(t *testing.T) {
	down := httptest.NewServer(http.NewServeMux())
	downURL := down.URL
	down.Close() // nothing is listening now → dial fails

	rr := httptest.NewRecorder()
	withBus(&fakeService{}, downURL).Routes().ServeHTTP(
		rr, httptest.NewRequest(http.MethodGet, "/events/stream?session=o/r-1", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

// Emit publishes a user.emit so workflow channels can deliver the message by
// type include.
func TestEmit_PublishesUserEmit(t *testing.T) {
	svc := &fakeService{}
	rec := postForm(t, New(svc).Routes(), "/events", url.Values{
		"session": {"owner/repo-1"},
		"body":    {"deploy is green\nfollow-up later"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if svc.gotPublish == nil {
		t.Fatal("PublishEvent was not called")
	}
	if svc.gotPublishFor != "owner/repo-1" {
		t.Errorf("session = %q", svc.gotPublishFor)
	}
	p := svc.gotPublish
	if p.Type != event.TypeUserEmit || p.Source != event.SourceWeb {
		t.Errorf("params = %+v", p)
	}
	if p.Summary != "deploy is green" {
		t.Errorf("summary = %q, want first line", p.Summary)
	}
	if p.Body != "deploy is green\nfollow-up later" {
		t.Errorf("body = %q", p.Body)
	}
}

// A non-user.* type is rejected before the service is touched (the web
// credential must not be able to forge privileged events).
func TestEmit_RejectsNonUserType(t *testing.T) {
	svc := &fakeService{}
	rec := postForm(t, New(svc).Routes(), "/events", url.Values{
		"session": {"owner/repo-1"},
		"type":    {"claude.permission_request"},
		"body":    {"y abc"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if svc.gotPublish != nil {
		t.Error("PublishEvent must not be called for a forbidden type")
	}
}

func TestEmit_RequiresSessionAndBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		form url.Values
	}{
		{"no session", url.Values{"body": {"x"}}},
		{"no body", url.Values{"session": {"o/r-1"}, "body": {"   "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeService{}
			rec := postForm(t, New(svc).Routes(), "/events", tc.form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if svc.gotPublish != nil {
				t.Error("PublishEvent must not be called on invalid input")
			}
		})
	}
}
