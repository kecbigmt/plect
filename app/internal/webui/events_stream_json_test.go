package webui

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/contracts/event"
)

func TestEventsStreamJSON_RequiresSession(t *testing.T) {
	rr := get(t, &fakeService{}, "/api/v1/events/stream")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

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

func fakeBusJSON(t *testing.T, wantSince, streamID string) *httptest.Server {
	t.Helper()
	if streamID == "" {
		streamID = "01GEN000"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); wantSince != "" && got != wantSince {
			t.Errorf("bus received since=%q, want %q", got, wantSince)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(": ping\n\n"))
		fmt.Fprintf(w, "id: %s:128\ndata: {\"id\":\"E1\",\"session_name\":\"acme/session-x\",\"type\":\"acme.reply\",\"source\":\"acme\",\"summary\":\"hi there\"}\n\n", streamID)
		w.(http.Flusher).Flush()
	})
	return httptest.NewServer(mux)
}

func TestEventsStreamJSON_RelaysEventsWithOpaqueResumeCursor(t *testing.T) {
	bus := fakeBusJSON(t, "", "")
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
			if id == "128" {
				t.Errorf("resume id = %q, want an opaque cursor, not the bus's raw offset", id)
			}
			sawCursorID = true
		case strings.Contains(line, `"summary":"hi there"`):
			sawEvent = true
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

func TestEventsStreamJSON_ResumesBusFromDecodedCursor(t *testing.T) {
	bus := fakeBusJSON(t, "01GEN000:64", "01GEN000")
	defer bus.Close()

	var gotCursors []string
	svc := &fakeService{resumeFn: func(_, cursor string) (string, int64, error) {
		gotCursors = append(gotCursors, cursor)
		return "01GEN000", 64, nil
	}}
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
	if len(gotCursors) != 1 || gotCursors[0] != "some-opaque-token" {
		t.Errorf("gotCursors = %v, want exactly one call decoding the request's cursor", gotCursors)
	}
}

func TestEventsStreamJSON_FrameCursorCarriesTheBusFrameStreamID(t *testing.T) {
	bus := fakeBusJSON(t, "", "01REAL000")
	defer bus.Close()

	svc := &fakeService{resumeGen: "01CONNECT"}
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

	var gotCursor string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if id, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
			gotCursor = id
		}
	}
	if gotCursor == "" {
		t.Fatal("no resume id was emitted")
	}
	decoded, err := event.DecodeCursor(gotCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if decoded.StreamID != "01REAL000" {
		t.Errorf("frame cursor stream id = %q, want the bus frame's own stream id %q, not the connect-time one (%q)", decoded.StreamID, "01REAL000", "01CONNECT")
	}
}

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
