package webapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
	"github.com/kecbigmt/plecture/contracts/event"
)

// SessionReader is the seam between this package's HTTP handlers and the
// service layer — the same calls webui.SessionService already exposes,
// kept as their own minimal interface so this package does not need to
// depend on webui (webui mounts Routes into its own mux instead).
type SessionReader interface {
	List() ([]service.ListEntry, error)
	Status(name string) (*service.StatusResult, error)
	// EventPage returns one page of the named session's event history,
	// unchanged from service.EventPage — this package adapts its wire
	// representation only, never its paging/cursor semantics.
	EventPage(session string, p service.EventPageParams) (service.EventPageResult, error)
}

// Routes mounts this slice's JSON API. Callers wanting it under a version
// prefix (this API's own convention is /api/v1, declared in the TypeSpec
// source's @server) strip that prefix before dispatch, mirroring how
// webui.Server composes its own mux.
func Routes(svc SessionReader) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", handleList(svc))
	// Session names contain "/", and a generated client sends it
	// percent-encoded (%2F) rather than literal, since this contract's
	// @typespec/openapi3 version does not emit OpenAPI 3's own
	// allowReserved parameter property — see web/api/README.md. This route
	// captures everything after the prefix rather than one path segment
	// (the same wildcard shape webui's own routes already use), so it
	// matches against net/http's already percent-decoded URL.Path and
	// resolves either form to the same name regardless.
	mux.HandleFunc("GET /sessions/{name...}", handleGet(svc))
	// session rides as a query param, not a path segment appended to
	// /sessions/{name...} above: a session name whose last segment happened
	// to literally be "events" would otherwise collide with that wildcard
	// route, silently reinterpreting a session-detail request as an event-
	// history request. This mirrors the existing bus (GET /v1/events?session=)
	// and Go-templated webui (GET /events?session=) surfaces, which carry the
	// same name for the same reason.
	mux.HandleFunc("GET /events", handleSessionEvents(svc))
	return mux
}

func handleList(svc SessionReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := svc.List()
		if err != nil {
			writeError(w, err)
			return
		}
		items := make([]webapiv1.SessionSummary, len(entries))
		for i, e := range entries {
			items[i] = summaryFromListEntry(e)
		}
		writeJSON(w, http.StatusOK, webapiv1.SessionListResponse{
			Items: items,
			Count: int32(len(items)),
		})
	}
}

func handleGet(svc SessionReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(r.PathValue("name"), "/")
		if name == "" {
			writeJSON(w, http.StatusNotFound, webapiv1.NotFoundError{
				Category: webapiv1.NotFoundErrorCategoryNotFound,
				Code:     webapiv1.SessionNotFound,
				Message:  "session name is required",
			})
			return
		}
		result, err := svc.Status(name)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, detailFromStatus(result))
	}
}

// event.Filter/eventlog.List treat Limit<=0 as "unlimited" — a sentinel this
// endpoint must never forward verbatim, since the durable log is unbounded
// and survives destroy: an omitted or non-positive limit would otherwise
// have the server scan, retain, convert, and serialize the entire log in one
// response. defaultEventPageLimit is the page size used when the client
// specifies no positive limit; maxEventPageLimit caps an explicit request so
// a client cannot get the same unbounded read by asking for a very large
// number instead of a non-positive one.
const (
	defaultEventPageLimit = 100
	maxEventPageLimit     = 1000
)

// handleSessionEvents serves one page of a session's event history. Unlike
// handleGet, an unknown session is not rejected up front: service.EventPage
// resolves an identifier that names no known session by falling back to it
// verbatim and reading whatever log exists at that name (possibly none),
// mirroring the CLI's own "missing session reads as an empty log" behavior —
// so a session that was never created returns 200 with an empty page, not a
// 404.
func handleSessionEvents(svc SessionReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		session := q.Get("session")
		if session == "" {
			writeValidationError(w, "session is required")
			return
		}
		order, err := event.NormalizeOrder(q.Get("order"))
		if err != nil {
			writeValidationError(w, err.Error())
			return
		}
		limit := defaultEventPageLimit
		if raw := q.Get("limit"); raw != "" {
			n, cerr := strconv.Atoi(raw)
			if cerr != nil {
				writeValidationError(w, "limit must be an integer")
				return
			}
			// A non-positive value falls back to the default rather than
			// being forwarded: it is never "unlimited" at this boundary.
			if n > 0 {
				limit = n
			}
		}
		if limit > maxEventPageLimit {
			limit = maxEventPageLimit
		}

		page, err := svc.EventPage(session, service.EventPageParams{
			Order:  order,
			Cursor: q.Get("cursor"),
			Filter: event.Filter{Limit: limit},
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, eventPageFromResult(page))
	}
}

// writeValidationError writes a 400 for a request the handler itself rejects
// before calling the service (a missing/malformed query parameter) — there is
// no *service.Error to route through apiError for these, since the service was
// never called.
func writeValidationError(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, webapiv1.ValidationError{
		Category: webapiv1.ValidationErrorCategoryValidation,
		Code:     webapiv1.InvalidInput,
		Message:  msg,
	})
}

func writeError(w http.ResponseWriter, err error) {
	status, body := apiError(err)
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
