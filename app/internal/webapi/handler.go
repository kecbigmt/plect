package webapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/service"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
)

// SessionReader is the seam between this package's HTTP handlers and the
// service layer — the same two calls webui.SessionService already exposes,
// kept as their own minimal interface so this package does not need to
// depend on webui (webui mounts Routes into its own mux instead).
type SessionReader interface {
	List() ([]service.ListEntry, error)
	Status(name string) (*service.StatusResult, error)
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

func writeError(w http.ResponseWriter, err error) {
	status, body := apiError(err)
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
