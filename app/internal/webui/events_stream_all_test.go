package webui

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/contracts/event"
)

func TestAllSessionsEventsStreamJSON_RelaysMatchingEventsAcrossSessions(t *testing.T) {
	svc := &fakeService{
		tailAllFn: func(ctx context.Context, f event.Filter, fn func(event.Event)) error {
			if !f.Match(event.Event{Type: "lifecycle.up"}) {
				t.Error("filter must match a lifecycle.* event")
			}
			if !f.Match(event.Event{Type: event.TypeStatusMessage}) {
				t.Error("filter must match a status-message event")
			}
			if f.Match(event.Event{Type: event.TypeUserNote}) {
				t.Error("filter must not match an ordinary conversational event")
			}
			fn(event.Event{ID: "01A", SessionName: "team/a", Type: "lifecycle.up", Summary: "up"})
			fn(event.Event{ID: "01B", SessionName: "team/b", Type: event.TypeStatusMessage, Summary: "hi"})
			<-ctx.Done()
			return ctx.Err()
		},
	}
	srv := httptest.NewServer(New(svc).Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events/stream/all", nil)
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

	var sawA, sawB bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, `"sessionName":"team/a"`):
			sawA = true
		case strings.Contains(line, `"sessionName":"team/b"`):
			sawB = true
		}
	}
	if !sawA {
		t.Error("session a's event was not relayed")
	}
	if !sawB {
		t.Error("session b's event was not relayed")
	}
}
