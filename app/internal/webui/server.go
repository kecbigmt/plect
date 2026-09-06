// Package webui serves a read-only-plus-lifecycle web UI over the plect service
// layer. Handlers depend on SessionService (not service.* directly) so they can
// be exercised without git/runtime side tasks.
package webui

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/webapi"
	"github.com/kecbigmt/plecture/app/internal/webui/webapp"
	"github.com/kecbigmt/plecture/contracts/event"
)

//go:embed assets
var assetsFS embed.FS

// templates carries every {{define}} block plus the template helpers. Page
// templates live at the templates root; reusable partials live one-per-file
// under components/ (and generated icon partials under components/icons/), so
// the set scales without one growing file. We walk the tree rather than glob a
// fixed depth so new subdirectories are picked up for free.
var templates = template.Must(
	template.New("webui").
		Funcs(template.FuncMap{
			"statusClass":     statusClass,
			"buttonClass":     buttonClass,
			"dict":            dict,
			"sessionPath":     sessionPath,
			"subtreePath":     subtreePath,
			"queryEscape":     url.QueryEscape,
			"isWebURL":        isWebURL,
			"doneStatusClass": doneStatusClass,
			"doneSymbol":      doneSymbol,
			"taskName":        taskName,
			"leafStats":       leafStats,
			"gateSummary":     gateSummary,
		}).
		ParseFS(assetsFS, templateFiles()...),
)

// templateFiles returns every .html under assets/templates, recursively.
func templateFiles() []string {
	var files []string
	err := fs.WalkDir(assetsFS, "assets/templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".html") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		panic(err) // embedded FS walk can't fail at runtime; surface a bug loudly.
	}
	return files
}

// dict builds a map from alternating key/value pairs so component partials can
// be invoked with named props ({{template "button" (dict "variant" "outline" …)}}),
// working around html/template's lack of slots/keyword arguments.
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict: want an even number of args, got %d", len(pairs))
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %d is %T, want string", i, pairs[i])
		}
		m[k] = pairs[i+1]
	}
	return m, nil
}

// SessionService is the seam between HTTP handlers and the plect service layer.
// The live implementation wraps service.*; tests inject a fake. The lifecycle
// methods take the service.*Params structs directly (Observer left nil — the
// web has no progress spinner to drive), so the seam stays a thin pass-through.
type SessionService interface {
	List() ([]service.ListEntry, error)
	Status(name string) (*service.StatusResult, error)
	Events(name string) ([]event.Event, error)
	// EventsSubtree returns newest-first (unlike Events, which is a raw
	// oldest-first tail): the service-layer desc page is already the
	// timeline's display order, so handlers must not reverse it.
	EventsSubtree(root string) ([]event.Event, error)
	// EventPage satisfies webapi.SessionReader: the generated JSON API's
	// history endpoint (mounted below at /api/v1/events) is a thin pass-
	// through to the same service.EventPage this htmx UI's own timeline
	// reads never needed cursor-based paging for.
	EventPage(name string, p service.EventPageParams) (service.EventPageResult, error)
	EventStreamResume(name, cursor string) (gen string, offset int64, err error)
	PublishEvent(name string, p service.EventPublishParams) (event.Event, error)
	Create(service.CreateParams) (*service.CreateResult, error)
	Up(service.UpParams) (*service.UpResult, error)
	Down(service.DownParams) (*service.DownResult, error)
	Destroy(service.DestroyParams) (*service.DestroyResult, error)
}

// Server holds the service seam, the parsed templates, and the runtime config
// (auth token, used by the auth middleware).
type Server struct {
	svc  SessionService
	tmpl *template.Template
	cfg  *Config
	// busClientFn, when set, overrides how the live-timeline handler reaches the
	// bus (tests point it at an httptest bus instead of a Unix socket).
	busClientFn func() *event.Client
}

// New builds a Server with default config (no auth token — tailnet trust).
// Tests use it directly; production wires the loaded config via NewWithConfig.
func New(svc SessionService) *Server {
	return NewWithConfig(svc, &Config{})
}

// NewWithConfig builds a Server with the given runtime config.
func NewWithConfig(svc SessionService, cfg *Config) *Server {
	if cfg == nil {
		cfg = &Config{}
	}
	return &Server{svc: svc, tmpl: templates, cfg: cfg}
}

// Routes returns the HTTP handler with all routes registered, wrapped in the
// auth (outermost) and CSRF middleware so an unauthenticated request is
// rejected before any state-changing work.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /sessions", s.handleSessionsPartial)
	// Session names contain "/", so the detail route captures the rest of the
	// path with a {name...} wildcard.
	mux.HandleFunc("GET /sessions/{name...}", s.handleSessionDetail)
	// Event timeline partial. Session names contain "/", which collides with a
	// {name...} sub-route, so the scope rides as a query param instead
	// (session=, or the cross-session scope subtree=).
	mux.HandleFunc("GET /events", s.handleSessionEvents)
	// Cross-session timeline page: the session tree rooted at a session.
	mux.HandleFunc("GET /subtrees/{name...}", s.handleSubtreeTimeline)
	// Live timeline: plect-web opens the bus SSE stream server-side and relays it
	// to the browser (same-origin, so the browser holds no bus token / UDS).
	mux.HandleFunc("GET /events/stream", s.handleSessionEventsStream)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// The generated-contract JSON API (see the schema-contract ADR under
	// docs/adr/): Session list/detail, bounded event history, and common
	// errors, at this slice's fixed version prefix. s.svc already implements
	// webapi.SessionReader. No method restriction here — webapi.Routes owns
	// its own per-method dispatch (and 404/405 responses) as later tasks
	// add operations.
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", webapi.Routes(s.svc)))
	// Hand-written, not part of the generated Session contract above: the
	// browser shell's own startup needs (see bootstrap.go). A literal pattern
	// takes precedence over the "/api/v1/" subtree pattern it overlaps
	// (net/http's ServeMux routes the most specific match), so this still
	// resolves here regardless of registration order.
	mux.HandleFunc("GET /api/v1/bootstrap", s.handleBootstrap)
	// events_stream_json.go: generated contracts do not own SSE replay
	// semantics, so this route is hand-written like the bootstrap one above.
	mux.HandleFunc("GET /api/v1/events/stream", s.handleSessionEventsStreamJSON)

	// Lifecycle mutations. A {name...} wildcard must be the final path segment,
	// so the action can't be a suffix after the (slash-containing) name; the
	// session name travels in the form body instead.
	mux.HandleFunc("POST /sessions", s.handleSessionCreate)
	mux.HandleFunc("POST /sessions/up", s.handleSessionUp)
	mux.HandleFunc("POST /sessions/down", s.handleSessionDown)
	mux.HandleFunc("POST /sessions/destroy", s.handleSessionDestroy)
	// Emit: publish a user.* event from the detail page. The bus tailer fans the
	// appended event back to the live stream, so the timeline updates on its own.
	mux.HandleFunc("POST /events", s.handleSessionEventEmit)

	if s.cfg.AuthToken != "" {
		mux.HandleFunc("GET /login", s.handleLoginForm)
		mux.HandleFunc("POST /login", s.handleLoginSubmit)
	}

	staticFS, _ := fs.Sub(assetsFS, "assets/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	// The React shell's development/review entry (see webapp/embed.go and
	// the client/server boundary ADR). "/" above remains the production
	// entry until the documented cutover; this path carries no
	// compatibility promise beyond that milestone.
	mux.Handle("GET /app/", s.handleWebApp())

	return s.authMiddleware(s.csrfMiddleware(mux))
}

// render executes a named template with a 200 status.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	s.renderStatus(w, http.StatusOK, name, data)
}

// renderStatus executes a named template into a buffer first so a mid-render
// failure becomes a 500 rather than a half-written response, then writes it
// with the given status.
func (s *Server) renderStatus(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.renderError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) renderError(w http.ResponseWriter, err error) {
	s.renderStatusError(w, http.StatusInternalServerError, err.Error())
}

// renderStatusError writes an error banner with an explicit status. The banner
// is a fragment so htmx can swap it next to the form that triggered the failed
// mutation; the status carries the machine-readable outcome (400/403/404/...).
func (s *Server) renderStatusError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = s.tmpl.ExecuteTemplate(w, "error-banner", msg)
}

// httpStatusForError maps a service.Error code to an HTTP status so the web
// surfaces the same distinctions the CLI does (bad input vs forbidden repo vs
// missing session). Non-service errors are treated as 500.
func httpStatusForError(err error) int {
	var se *service.Error
	if errors.As(err, &se) {
		switch se.Code {
		case service.ErrInvalidURL, service.ErrInvalidTag, service.ErrInvalidInput:
			return http.StatusBadRequest
		case service.ErrRepoNotAllowed:
			return http.StatusForbidden
		case service.ErrSessionNotFound:
			return http.StatusNotFound
		case service.ErrNotAttachable, service.ErrNotProduced:
			return http.StatusConflict
		}
	}
	return http.StatusInternalServerError
}

// sessionPath builds the detail URL for a session. Names are session-N, so
// each "/"-separated segment is path-escaped while the separators are kept,
// yielding /sessions/<segment>/<segment> that the {name...} route reverses.
func sessionPath(name string) string {
	return "/sessions/" + escapeSegments(name)
}

// subtreePath builds the subtree-timeline URL for the tree rooted at a session.
func subtreePath(name string) string {
	return "/subtrees/" + escapeSegments(name)
}

func escapeSegments(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// isWebURL reports whether a resource identifier is a navigable http(s) URL.
// Resource ids are arbitrary strings (resolver-less ids like "my-experiment",
// or schemes like "acme:foo"), so the detail view only renders an anchor when
// the value is actually a web link — otherwise it shows plain text.
func isWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// statusClass maps a session status to its badge color classes; the badge's
// structural classes stay in the template. Per DESIGN.md these reuse Tailwind's
// built-in palette (not semantic tokens): the *-100/*-800 pairs are the
// WCAG-AA-safe combinations, which the neutral token set doesn't provide for
// success/warning.
func statusClass(s any) string {
	switch v := s.(type) {
	case domain.RunState:
		switch v {
		case domain.RunUp:
			return "bg-green-100 text-green-800"
		case domain.RunDown:
			return "bg-gray-100 text-gray-700"
		}
	case domain.HealthState:
		switch v {
		case domain.HealthHealthy:
			return "bg-green-100 text-green-800"
		case domain.HealthUnhealthy:
			return "bg-red-100 text-red-800"
		case domain.HealthStalled:
			return "bg-orange-100 text-orange-800"
		case domain.HealthUndeclared:
			return "bg-yellow-100 text-yellow-800"
		}
	}
	return "bg-gray-100 text-gray-700"
}

// buttonClass returns the full class string for a button variant: shared base
// (layout, focus ring, disabled) plus the variant's token-driven colors. An
// unknown or missing variant falls back to "default". It takes `any` because
// template dict values arrive as interface{} (which a string param won't accept).
func buttonClass(variant any) string {
	const base = "inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-md px-4 py-2 text-label font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 disabled:pointer-events-none disabled:opacity-50"
	name, _ := variant.(string)
	var v string
	switch name {
	case "destructive":
		v = "bg-destructive text-destructive-foreground hover:bg-destructive/90"
	case "outline":
		v = "border border-input bg-background hover:bg-accent hover:text-accent-foreground"
	case "ghost":
		v = "hover:bg-accent hover:text-accent-foreground"
	default: // "default" and anything unknown
		v = "bg-primary text-primary-foreground hover:bg-primary/90"
	}
	return base + " " + v
}

// handleWebApp serves the embedded React shell build under /app/. A path
// that does not resolve to a real built file (e.g. a future client-side
// route the SPA's own router owns) falls back to index.html instead of a
// bare 404, so a direct navigation or reload lands on the shell rather than
// an error page — the same reason any single-page app's server needs a
// catch-all.
func (s *Server) handleWebApp() http.Handler {
	dist, err := fs.Sub(webapp.FS, "dist")
	if err != nil {
		panic(err) // the embedded build always contains dist/; a missing one is a build bug, not a runtime condition.
	}
	fileServer := http.StripPrefix("/app/", http.FileServerFS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/app/")
		if name != "" {
			if _, err := fs.Stat(dist, name); err != nil {
				name = "" // unknown path: fall through to the directory below
			}
		}
		req := r.Clone(r.Context())
		if name == "" {
			// The bare directory: FileServerFS serves dist/index.html for
			// this on its own. Naming index.html explicitly here instead
			// would hit net/http's own "/index.html -> ./" canonicalizing
			// redirect, which a fetch() caller does not transparently follow
			// the way a browser navigation does.
			req.URL = &url.URL{Path: "/app/", RawQuery: r.URL.RawQuery}
		} else {
			req.URL = &url.URL{Path: "/app/" + name, RawQuery: r.URL.RawQuery}
		}
		fileServer.ServeHTTP(w, req)
	})
}
