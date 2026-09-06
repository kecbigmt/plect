package webui

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/webapi"
	"github.com/kecbigmt/plecture/contracts/event"
)

// handleSessionEventsStreamJSON is the React shell's live-timeline endpoint:
// GET /api/v1/events/stream?session=&cursor=. It is hand-written rather than
// part of the generated @plecture/web-api contract (see web/api/README.md's
// "What this PR does not claim" — SSE reconnection/replay semantics stay
// explicit and separately tested at this boundary), mounted as a literal
// pattern the same way GET /api/v1/bootstrap already is.
//
// Unlike the Go-templated timeline's handleSessionEventsStream (which relays
// rendered HTML rows and resumes from the bus's own raw byte-offset Last-
// Event-ID), this endpoint's resume token IS the opaque cursor
// GET /api/v1/events already returns as nextCursor: docs/design/web-ui-
// event-history.md's history/live handoff closes its race window by letting
// a client hand that cursor straight to this endpoint, with no second cursor
// format to translate. service.EventStreamResume decodes and validates it
// exactly like EventPage does, so a malformed, wrong-order, or stale-
// generation cursor is a 400 here the same way it already is there — the
// bus is never dialed with a resume position that cannot be trusted. No
// cursor at all is a fresh connect: it replays a bounded recent tail
// (streamTailLimit, unchanged) like the HTML relay does.
//
// Each frame carries one JSON object in the identical shape as the history
// endpoint's own events[] item (webapi.EventFromDomain — the same
// conversion, not a second one), and its `id:` is the same opaque cursor
// format re-encoded for the position after that record, so a client's own
// fetch-based reconnect module can hand that id straight back as this
// endpoint's `cursor` with no translation of its own.
func (s *Server) handleSessionEventsStreamJSON(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session == "" {
		writeAPIValidationError(w, "session is required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	gen, offset, err := s.svc.EventStreamResume(session, r.URL.Query().Get("cursor"))
	if err != nil {
		status, body := webapi.ApiError(err)
		writeJSONBody(w, status, body)
		return
	}

	resp, err := s.openBusStream(r.Context(), s.busClient(), session, offset)
	if err != nil {
		http.Error(w, "event bus unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	_ = relayBusBodyJSON(resp.Body, w, flusher, gen)
}

// relayBusBodyJSON copies a bus SSE stream to the browser, converting each
// event frame's payload to the wire's Event DTO and its raw byte-offset id
// to the opaque event.Cursor format — the same transformation
// relayBusBody (events_stream.go) applies for rendering, kept as a separate
// function since the two outputs (HTML row vs. JSON DTO) share no rendering
// path worth abstracting over. Comment lines (keepalives) are forwarded
// verbatim, exactly as relayBusBody does.
func relayBusBodyJSON(body io.Reader, w io.Writer, flusher http.Flusher, gen string) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var dataLines []string
	var lastOffset int64
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(dataLines) == 0 {
				continue
			}
			var ev event.Event
			if json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &ev) == nil {
				cursor := event.Cursor{V: event.CursorVersion, Off: lastOffset, Ord: event.OrderAsc, Gen: gen}.Encode()
				payload, merr := json.Marshal(webapi.EventFromDomain(ev))
				if merr == nil {
					if err := writeEventFrame(w, cursor, string(payload)); err != nil {
						return err
					}
					flusher.Flush()
				}
			}
			dataLines = dataLines[:0]
		case strings.HasPrefix(line, ":"):
			if _, err := io.WriteString(w, line+"\n\n"); err != nil {
				return err
			}
			flusher.Flush()
		case strings.HasPrefix(line, "id:"):
			lastOffset, _ = strconv.ParseInt(strings.TrimSpace(line[3:]), 10, 64)
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	return sc.Err()
}

// writeAPIValidationError writes the JSON API's ValidationError shape for a
// request this handler rejects before calling the service (a missing query
// parameter) — mirrors webapi's own writeValidationError, which is
// unexported and scoped to that package's generated-contract handlers. A
// *service.Error routes through webapi.ApiError's own classification table
// rather than a second, hand-built error envelope.
func writeAPIValidationError(w http.ResponseWriter, msg string) {
	status, body := webapi.ApiError(&service.Error{Code: service.ErrInvalidInput, Message: msg})
	writeJSONBody(w, status, body)
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
