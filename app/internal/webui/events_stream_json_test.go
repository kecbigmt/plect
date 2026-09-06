package webui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/service"
)

func TestEventsStreamJSON_RequiresSession(t *testing.T) {
	rr := get(t, &fakeService{}, "/api/v1/events/stream")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// A malformed/stale-generation cursor is rejected as a 400 before the bus is
// ever dialed — the same "invalid resume state" semantics EventPage already
// applies, not a silently-wrong resume position.
func TestEventsStreamJSON_RejectsInvalidCursor(t *testing.T) {
	svc := &fakeService{resumeErr: &service.Error{Code: service.ErrInvalidInput, Message: "cursor expired"}}
	rr := get(t, svc, "/api/v1/events/stream?session=acme/session-x&cursor=garbage")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body)
	}
	if svc.gotResumeCursor != "garbage" {
		t.Errorf("resume cursor = %q, want the request's cursor forwarded to EventStreamResume", svc.gotResumeCursor)
	}
}

// fakeBusJSON serves GET /v1/stream with one keepalive comment and one event
// frame at a known byte offset, so the relay's cursor re-encoding can be
// checked against a known generation.
func fakeBusJSON(t *testing.T, wantSince string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); wantSince != "" && got != wantSince {
			t.Errorf("bus received since=%q, want %q", got, wantSince)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(": ping\n\n"))
		_, _ = w.Write([]byte("id: 128\ndata: {\"id\":\"E1\",\"session_name\":\"acme/session-x\",\"type\":\"acme.reply\",\"source\":\"acme\",\"summary\":\"hi there\"}\n\n"))
		w.(http.Flusher).Flush()
	})
	return httptest.NewServer(mux)
}

// A fresh connect (no cursor) forwards no since= to the bus and relays each
// frame as JSON identical in shape to the history endpoint's own Event DTO,
// with the resume id re-encoded as the same opaque cursor format EventPage
// returns as NextCursor (not the bus's raw byte offset).
func TestEventsStreamJSON_RelaysEventsWithOpaqueResumeCursor(t *testing.T) {
	bus := fakeBusJSON(t, "")
	defer bus.Close()

	svc := &fakeService{resumeGen: "01GEN000"}
	srv := httptest.NewServer(withBus(svc, bus.URL).Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events/stream?session=acme/session-x", nil)
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

	var sawPing, sawEvent, sawCursorID bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ": ping"):
			sawPing = true
		case strings.HasPrefix(line, "id: "):
			id := strings.TrimPrefix(line, "id: ")
			// The resume id must be the opaque cursor format, not the bus's
			// raw "128" byte offset — a real cursor decodes back to it.
			if id == "128" {
				t.Errorf("resume id = %q, want an opaque cursor, not the bus's raw offset", id)
			}
			sawCursorID = true
		case strings.Contains(line, `"summary":"hi there"`):
			sawEvent = true
			// The DTO uses the wire's camelCase sessionName field, matching
			// webapi.EventFromDomain/webapiv1.Event exactly (see
			// testdata/event_page.valid.json).
			if !strings.Contains(line, `"sessionName":"acme/session-x"`) {
				t.Errorf("frame = %q, want the same field shape as webapiv1.Event", line)
			}
		}
	}
	if !sawPing {
		t.Error("keepalive comment was not forwarded")
	}
	if !sawCursorID {
		t.Error("resume id was not forwarded")
	}
	if !sawEvent {
		t.Error("event was not relayed as JSON")
	}
}

// A resume cursor decodes to a byte offset that becomes the bus's own
// since= parameter — the live endpoint's resume mechanism is EventPage's
// cursor, translated, not a second format.
func TestEventsStreamJSON_ResumesBusFromDecodedCursor(t *testing.T) {
	bus := fakeBusJSON(t, "64")
	defer bus.Close()

	svc := &fakeService{resumeGen: "01GEN000", resumeOffset: 64}
	srv := httptest.NewServer(withBus(svc, bus.URL).Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events/stream?session=acme/session-x&cursor=some-opaque-token", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body would confirm since= assertion inside fakeBusJSON", resp.StatusCode)
	}
	_, _ = bufio.NewReader(resp.Body).ReadString('\n') // drain enough to let the handler run
	if svc.gotResumeCursor != "some-opaque-token" {
		t.Errorf("resume cursor = %q", svc.gotResumeCursor)
	}
}

// When the bus is unreachable the proxy returns a 502 before committing the
// stream's 200, matching the existing HTML relay's behavior.
func TestEventsStreamJSON_BusUnavailable(t *testing.T) {
	down := httptest.NewServer(http.NewServeMux())
	downURL := down.URL
	down.Close()

	rr := httptest.NewRecorder()
	withBus(&fakeService{}, downURL).Routes().ServeHTTP(
		rr, httptest.NewRequest(http.MethodGet, "/api/v1/events/stream?session=acme/session-x", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}
