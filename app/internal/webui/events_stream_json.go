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

	// Re-resolved on every call rather than cached: a destroy + same-name
	// recreate mints a new stream mid-connection, and a cursor still
	// carrying the superseded id would validate against the wrong
	// incarnation on the browser's next reconnect.
	known := gen
	resolveGen := func() string {
		if g, _, gerr := s.svc.EventStreamResume(session, ""); gerr == nil {
			known = g
		}
		return known
	}

	_ = relayBusBodyJSON(resp.Body, w, flusher, resolveGen)
}

func relayBusBodyJSON(body io.Reader, w io.Writer, flusher http.Flusher, resolveGen func() string) error {
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
				cursor := event.Cursor{V: event.CursorVersion, Off: lastOffset, Ord: event.OrderAsc, StreamID: resolveGen()}.Encode()
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

func writeAPIValidationError(w http.ResponseWriter, msg string) {
	status, body := webapi.ApiError(&service.Error{Code: service.ErrInvalidInput, Message: msg})
	writeJSONBody(w, status, body)
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
