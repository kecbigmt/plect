package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/contracts/event"
)

// busClient builds a fresh bus client per request from the configured socket
// and token. The client is cheap (it only wires a UDS-dialing http.Client) and
// stateless, so there is nothing to share across requests. Tests set
// busClientFn to point at an httptest bus instead of a real Unix socket.
func (s *Server) busClient() *event.Client {
	if s.busClientFn != nil {
		return s.busClientFn()
	}
	return event.NewUDSClient(s.cfg.BusSocket, s.cfg.BusToken)
}

// streamTailLimit caps how many recent events a fresh live stream replays (it
// reuses the polling timeline's cap). A reconnect resumes from its cursor and
// ignores this. The durable log is unbounded and survives destroy, so an
// unscoped replay could flood a newly-opened pane.
const streamTailLimit = eventTimelineLimit

// handleSessionEventsStream is the browser-facing SSE endpoint for the live
// timeline. The browser's EventSource cannot dial the bus's Unix socket nor
// send a bearer header, so plect-web opens the bus stream server-side and relays
// each event as a rendered <li> fragment (same-origin, no token in the browser).
//
// A fresh connect replays the most recent streamTailLimit events then follows
// live; a reconnect's Last-Event-ID is this endpoint's own validated cursor, so the stream resumes there with no gap and no
// re-delivery. The bus is dialed before any 200 is written, so an unreachable
// bus surfaces as a 502 the browser's EventSource onerror can react to rather
// than a silently stalled stream.
func (s *Server) handleSessionEventsStream(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()

	var since int64
	var gen string
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if g, seq, err := s.svc.EventStreamResume(session, v); err == nil {
			gen, since = g, seq
		}
	}

	resp, err := s.openBusStream(ctx, s.busClient(), session, since)
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

	known := gen
	resolveGen := func() string {
		if known == "" {
			if g, _, gerr := s.svc.EventStreamResume(session, ""); gerr == nil {
				known = g
			}
		}
		return known
	}

	// Relay until the bus stream ends or the browser disconnects (ctx cancel).
	// Either way the browser's EventSource reconnects on its own, resuming from
	// the last id we emitted.
	_ = s.relayBusBody(resp.Body, w, flusher, resolveGen)
}

// openBusStream opens one bus SSE connection. A fresh connect (since==0) asks
// for the recent tail; a reconnect resumes from its sequence. It returns the
// response only on a 200, so the caller can map a failure to a 502 before
// committing the stream's own 200.
func (s *Server) openBusStream(ctx context.Context, c *event.Client, session string, since int64) (*http.Response, error) {
	q := url.Values{}
	q.Set("session", session)
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	} else {
		q.Set("tail", strconv.Itoa(streamTailLimit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/stream?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if since > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(since, 10))
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("bus stream: %s", resp.Status)
	}
	return resp, nil
}

// relayBusBody copies a bus SSE stream to the browser, rendering each event JSON
// frame into a timeline <li> and forwarding the bus's comment lines verbatim
// (": connected" / ": ping") so an idle browser connection stays warm. It
// returns when the bus stream ends, the context is cancelled, or a browser write
// fails. resolveGen re-encodes each frame's raw bus sequence as an opaque cursor.
func (s *Server) relayBusBody(body io.Reader, w io.Writer, flusher http.Flusher, resolveGen func() string) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var dataLines []string
	var lastSeq int64
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // blank line dispatches the buffered frame
			if len(dataLines) == 0 {
				continue
			}
			var ev event.Event
			if json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &ev) == nil {
				cursor := event.Cursor{V: event.CursorVersion, Off: lastSeq, Ord: event.OrderAsc, StreamID: resolveGen()}.Encode()
				if err := writeEventFrame(w, cursor, s.renderEventRow(ev)); err != nil {
					return err
				}
				flusher.Flush()
			}
			dataLines = dataLines[:0]
		case strings.HasPrefix(line, ":"): // comment (keepalive) — forward verbatim
			if _, err := io.WriteString(w, line+"\n\n"); err != nil {
				return err
			}
			flusher.Flush()
		case strings.HasPrefix(line, "id:"):
			lastSeq, _ = strconv.ParseInt(strings.TrimSpace(line[3:]), 10, 64)
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	return sc.Err()
}

// renderEventRow renders one event as the timeline's <li> partial. A render
// failure yields "" so the caller simply skips that frame.
func (s *Server) renderEventRow(ev event.Event) string {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "event-row", ev); err != nil {
		return ""
	}
	return buf.String()
}

// writeEventFrame emits an SSE frame carrying an HTML fragment. The fragment's
// newlines are split across multiple data: lines (the SSE wire format), which
// EventSource rejoins with "\n"; the id is the resume cursor for reconnects.
func writeEventFrame(w io.Writer, id, html string) error {
	if strings.TrimSpace(html) == "" {
		return nil
	}
	var b strings.Builder
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	for _, ln := range strings.Split(html, "\n") {
		b.WriteString("data: ")
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// handleSessionEventEmit publishes a user.* event from the detail page's emit
// form. The web credential may only publish user.* types (a privileged,
// provider-owned type must not be forgeable from a browser); the appended
// event reaches the open timeline via the bus tailer, so a successful emit
// just clears the form's error slot.
func (s *Server) handleSessionEventEmit(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("session"))
	if name == "" {
		s.renderStatusError(w, http.StatusBadRequest, "session is required")
		return
	}
	typ := strings.TrimSpace(r.FormValue("type"))
	if typ == "" {
		typ = event.TypeUserEmit
	}
	if !strings.HasPrefix(typ, "user.") {
		s.renderStatusError(w, http.StatusForbidden, "web may only emit user.* events")
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		s.renderStatusError(w, http.StatusBadRequest, "message is required")
		return
	}
	summary := firstLine(body)
	if _, err := s.svc.PublishEvent(name, service.EventPublishParams{
		Type:      typ,
		Source:    event.SourceWeb,
		Direction: event.Internal,
		Summary:   summary,
		Body:      body,
	}); err != nil {
		s.renderStatusError(w, httpStatusForError(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusOK) // empty body clears #emit-error; the timeline updates via SSE
}

// firstLine is the event's one-line summary: the first line of the body,
// trimmed and capped to a sane rune count (the UI is Japanese, so truncate on
// rune boundaries to avoid splitting a multibyte character).
func firstLine(body string) string {
	line := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		line = body[:i]
	}
	line = strings.TrimSpace(line)
	const max = 120
	if r := []rune(line); len(r) > max {
		return string(r[:max]) + "…"
	}
	return line
}
