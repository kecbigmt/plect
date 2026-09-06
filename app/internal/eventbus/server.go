// Package eventbus is the live pub/sub fan-out server behind `plect serve`.
//
// The durable per-session log (app/internal/eventlog) is the source of truth;
// this server is a thin HTTP/SSE face over it:
//
//	POST /v1/events           append an event (the only write path)
//	GET  /v1/events           list a session's events (replay/paging)
//	GET  /v1/stream           SSE: replay from a cursor, then follow live
//
// session rides as a query param (not a path segment) to avoid the %2F-in-path
// problem for names like "session-1". The log is the only fan-out path —
// every subscriber follows it by polling from its cursor — so events appended
// via POST here OR directly by another plect process (CLI/sync/web/capture) are
// delivered exactly once, with no separate in-memory broadcast to double-deliver.
package eventbus

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/contracts/event"
)

// keepAliveInterval bounds how long a stream stays silent: after this much idle
// time the loop sends an SSE comment (": ping") so an idle browser/proxy
// connection (and any intermediary) is not torn down for inactivity. It is a var
// only so tests can shorten it.
var keepAliveInterval = 15 * time.Second

// LiveTail is the per-session shared reader an SSE stream subscribes to instead
// of running its own poll loop (sessionhub.Registry implements it).
type LiveTail interface {
	SubscribeFrames(session string) *sessionhub.FrameSub
}

// Server serves the bus HTTP/SSE API over a durable event log.
type Server struct {
	store *eventlog.Store
	token string // bearer token; "" disables auth (rely on the UDS 0600 perms)
	hub   LiveTail
}

// New builds a Server over the given log store. token, if non-empty, is required
// as `Authorization: Bearer <token>` on every endpoint except /healthz.
func New(store *eventlog.Store, token string, hub LiveTail) *Server {
	return &Server{store: store, token: token, hub: hub}
}

// Routes returns the HTTP handler with all bus routes registered.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.handlePublish)
	mux.HandleFunc("GET /v1/events", s.handleList)
	mux.HandleFunc("GET /v1/stream", s.handleStream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	want := "Bearer " + s.token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var ev event.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if ev.SessionName == "" {
		http.Error(w, "session_name is required", http.StatusBadRequest)
		return
	}
	stored, off, _, err := s.store.Append(ev)
	if err != nil {
		http.Error(w, "append failed", http.StatusInternalServerError) // avoid leaking FS paths
		return
	}
	writeJSON(w, map[string]any{"id": stored.ID, "offset": off})
}

// handleList returns one page of a session's events using opaque cursors (the
// same contract as service.EventPage / the CLI): asc paginates forward from the
// head (or ?cursor) and returns next_cursor; desc returns the most recent page
// and does not paginate in v1. A cursor is validated against the requested order
// and the log's current stream id, so a stale or cross-order token is rejected
// rather than silently resolving to the wrong record. The opaque event.Cursor
// token is decoded here into the underlying sequence; the live stream
// (handleStream) exposes that raw sequence directly instead, since it is an
// internal protocol between plect processes and the resident bus, never a
// browser-facing surface.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	order, err := event.NormalizeOrder(r.URL.Query().Get("order"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	streamID, err := s.store.StreamID(session)
	if err != nil {
		http.Error(w, "list failed", http.StatusInternalServerError) // avoid leaking FS paths
		return
	}
	f := parseFilter(r)

	var since int64
	if token := r.URL.Query().Get("cursor"); token != "" {
		cur, derr := event.DecodeCursor(token)
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusBadRequest)
			return
		}
		if verr := cur.Validate(order, streamID); verr != nil {
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return
		}
		since = cur.Off
	}

	if order == event.OrderDesc {
		evs, lerr := s.store.Tail(session, f, f.Limit)
		if lerr != nil {
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		slices.Reverse(evs)
		if evs == nil {
			evs = []event.Event{}
		}
		writeJSON(w, map[string]any{"events": evs, "next_cursor": ""})
		return
	}

	evs, _, next, lerr := s.store.List(session, since, f)
	if lerr != nil {
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	if evs == nil {
		evs = []event.Event{}
	}
	// A forward cursor only makes sense once the log exists (streamID != "");
	// without it there is nothing to resume against, so next_cursor stays empty.
	nextCursor := ""
	if streamID != "" {
		nextCursor = event.Cursor{V: event.CursorVersion, Off: next, Ord: event.OrderAsc, StreamID: streamID}.Encode()
	}
	writeJSON(w, map[string]any{"events": evs, "next_cursor": nextCursor})
}

// handleStream is an SSE endpoint: it replays the session's events from the
// cursor (Last-Event-ID header or ?since) then follows the log live. Each frame
// carries `id: <resume sequence>` (the sequence past that record) so a
// reconnect with Last-Event-ID resumes with no gap and no re-delivery. This is
// an internal protocol between plect processes and the resident bus — a raw
// sequence, not the browser-facing opaque cursor a webui relay re-encodes it
// as before ever forwarding it to a browser.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
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
	f := parseFilter(r)
	f.Limit = 0 // a live stream is unbounded; Limit only applies to list paging

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	start := sinceParam(r)
	// A fresh stream (no resume cursor) with ?tail=N replays only the most recent
	// N matching records instead of the whole log — the durable log is unbounded
	// and survives destroy, so an unscoped replay could flood a new subscriber.
	// A reconnect carries a cursor and skips this (it resumes exactly).
	if start == 0 {
		if tail := tailParam(r); tail > 0 {
			if off, err := s.store.TailOffset(session, f, tail); err == nil {
				start = off
			}
		}
	}

	// Join the session's shared reader, then replay history up to the join
	// boundary; the live stream owns everything at/after it. Capturing the
	// boundary at subscribe time (under the reader's lock) makes the handoff
	// gap-free and dup-free.
	sub := s.hub.SubscribeFrames(session)
	defer sub.Close()
	boundary := sub.Start()

	cursor := start
	for cursor < boundary {
		evs, offs, next, err := s.store.List(session, cursor, f)
		if err != nil {
			return
		}
		for i := range evs {
			if offs[i] >= boundary {
				break // the live stream delivers records at/after the boundary
			}
			resume := next
			if i+1 < len(offs) {
				resume = offs[i+1] // resume point = start of the next delivered record
			}
			b, _ := json.Marshal(evs[i])
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", resume, b)
		}
		if next <= cursor {
			break
		}
		cursor = min(next, boundary)
	}
	flusher.Flush()

	ka := time.NewTimer(keepAliveInterval)
	defer ka.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case fr, ok := <-sub.Frames():
			if !ok {
				return // reader dropped a slow consumer; the client reconnects via Last-Event-ID
			}
			// Skip what replay already covered (a Last-Event-ID ahead of the join
			// boundary) and apply the request's type filter (the reader is unfiltered).
			if fr.Start < start || !f.Match(fr.Event) {
				continue
			}
			// fr.Resume is the offset past this record. On a filtered stream that is
			// the post-record offset rather than the next-matching one; both resume
			// correctly (a reconnect re-reads and re-filters from the Last-Event-ID).
			b, _ := json.Marshal(fr.Event)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", fr.Resume, b)
			flusher.Flush()
			if !ka.Stop() {
				select {
				case <-ka.C:
				default:
				}
			}
			ka.Reset(keepAliveInterval)
		case <-ka.C:
			io.WriteString(w, ": ping\n\n")
			flusher.Flush()
			ka.Reset(keepAliveInterval)
		}
	}
}

// tailParam reads ?tail=N (the number of most-recent records a fresh stream
// should replay); 0 or absent means replay everything from the cursor.
func tailParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if n < 0 {
		return 0
	}
	return n
}

func sinceParam(r *http.Request) int64 {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	n, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	return n
}

func parseFilter(r *http.Request) event.Filter {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return event.Filter{
		Types:        event.SplitCSV(q.Get("types")),
		Sources:      event.SplitCSV(q.Get("source")),
		Direction:    event.Direction(q.Get("direction")),
		DeliveryMode: event.DeliveryMode(q.Get("delivery_mode")),
		Limit:        limit,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
