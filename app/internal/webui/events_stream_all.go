package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/kecbigmt/plecture/app/internal/webapi"
	"github.com/kecbigmt/plecture/contracts/event"
)

// The two namespaces a session-list row can show (run/health, self-reported
// message); everything else is a per-session concern the selected session's
// own stream already covers.
var allSessionsStreamFilter = event.Filter{Types: []string{event.TypeLifecyclePrefix + "*", event.TypeStatusMessage}}

// Unlike the per-session stream, this one is not relayed through the bus, so
// nothing else keeps an idle connection warm through a proxy.
var allSessionsKeepAliveInterval = 15 * time.Second

// handleAllSessionsEventsStreamJSON is the list's live-update source. It has
// no resume cursor: a reconnect just replays the filtered history again,
// acceptable since the client coalesces its reaction regardless of volume.
func (s *Server) handleAllSessionsEventsStreamJSON(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events := make(chan event.Event)
	tailDone := make(chan error, 1)
	go func() {
		tailDone <- s.svc.EventTailAll(ctx, allSessionsStreamFilter, func(ev event.Event) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		})
	}()

	ka := time.NewTicker(allSessionsKeepAliveInterval)
	defer ka.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tailDone:
			return
		case ev := <-events:
			payload, merr := json.Marshal(webapi.EventFromDomain(ev))
			if merr != nil {
				continue
			}
			if err := writeEventFrame(w, ev.ID, string(payload)); err != nil {
				return
			}
			flusher.Flush()
			ka.Reset(allSessionsKeepAliveInterval)
		case <-ka.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
