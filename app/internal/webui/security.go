package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Cookie / header names for the two independent defenses: CSRF (always on) and
// auth_token (on only when configured).
const (
	csrfCookieName = "plect_csrf"
	csrfHeaderName = "X-CSRF-Token"
	csrfFormField  = "csrf_token"
	authCookieName = "plect_auth"
)

// ensureCSRFToken returns the request's CSRF token, minting and setting a
// SameSite=Strict cookie when absent. GET handlers that render a page with
// forms call this and inject the token into the page (via the body's
// hx-headers), so every later htmx mutation carries it back in csrfHeaderName.
func (s *Server) ensureCSRFToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	tok := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	return tok
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms.
	return hex.EncodeToString(b)
}

// csrfMiddleware rejects unsafe-method requests that fail same-origin OR
// double-submit token validation. /login is exempt: it is the pre-auth entry
// point (the user may not hold a CSRF cookie yet) and is independently
// protected by a SameSite=Strict auth cookie and a single-user threat model.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUnsafeMethod(r.Method) && r.URL.Path != "/login" && !validCSRF(r) {
			const msg = "CSRF validation failed; reload the page and retry"
			if isAPIPath(r.URL.Path) {
				writeAPIAuthError(w, http.StatusForbidden, msg)
				return
			}
			s.renderStatusError(w, http.StatusForbidden, msg)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAPIPath marks the JSON API namespace. A request under it must never
// receive an HTML redirect or error banner on an auth/CSRF failure: a JSON
// client that followed a 303 to /login would parse that page's HTML as if
// it were the API response it asked for — the "unexpectedly rendering HTML
// to JSON consumers" failure the React shell's bootstrap must not hit.
func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/")
}

// writeAPIAuthError writes the minimal JSON body for a middleware-level
// auth/CSRF rejection under isAPIPath. It deliberately does not reuse
// webapi.ApiError's category enum: that type discriminates service.Error
// outcomes from a request that reached the service layer, while this
// rejection happens in front of it and carries no service.Error code.
func writeAPIAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
	}{Message: message})
}

func isUnsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// validCSRF requires both a same-origin request and a token in the request that
// matches the cookie (double submit). The cookie is SameSite=Strict and
// HttpOnly, so a cross-site page can neither read nor send it, and the token it
// would need to echo is unknowable; the origin check is belt-and-suspenders for
// clients that send Origin/Referer.
func validCSRF(r *http.Request) bool {
	if !sameOrigin(r) {
		return false
	}
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	sent := r.Header.Get(csrfHeaderName)
	if sent == "" {
		sent = r.PostFormValue(csrfFormField) // fallback for a non-htmx form post
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(sent)) == 1
}

// sameOrigin verifies the request's Origin (or Referer) host matches the Host
// it was sent to. A missing Origin/Referer on an unsafe method is treated as a
// failure: same-origin browser form/htmx posts always carry one.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// authMiddleware gates the whole app behind cfg.AuthToken when set — defense in
// depth for exposing the control plane over a private network/VPN. With no token configured
// it is a no-op (tailnet trust). /login, /static, and /healthz stay reachable
// so the login page can render and liveness checks work.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	if s.cfg.AuthToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExempt(r.URL.Path) || s.authed(r) {
			next.ServeHTTP(w, r)
			return
		}
		const msg = "authentication required"
		if isAPIPath(r.URL.Path) {
			writeAPIAuthError(w, http.StatusUnauthorized, msg)
			return
		}
		// A browser navigation goes to the login form, carrying where it was
		// headed so a successful login returns there; a programmatic/htmx call
		// gets a 401 it can surface directly.
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		s.renderStatusError(w, http.StatusUnauthorized, msg)
	})
}

func authExempt(path string) bool {
	return path == "/login" || path == "/healthz" || strings.HasPrefix(path, "/static/")
}

// authed accepts either an Authorization: Bearer header (for API/CLI callers)
// or the auth cookie set by the login form (for browsers).
func (s *Server) authed(r *http.Request) bool {
	want := []byte(s.cfg.AuthToken)
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), want) == 1 {
			return true
		}
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		return subtle.ConstantTimeCompare([]byte(c.Value), want) == 1
	}
	return false
}

// loginView is the "login" template's argument: whether to show the
// wrong-token banner, and where a successful login should return to.
type loginView struct {
	HasError bool
	Next     string
}

// handleLoginForm renders the token entry page. Registered only when an auth
// token is configured.
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login", loginView{Next: safeNextPath(r.URL.Query().Get("next"))})
}

// handleLoginSubmit sets the auth cookie when the posted token matches, then
// redirects to next (the page authMiddleware sent the caller here from,
// defaulting to the list); a wrong token re-renders the form with an error.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	next := safeNextPath(r.FormValue("next"))
	token := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AuthToken)) != 1 {
		s.renderStatus(w, http.StatusUnauthorized, "login", loginView{HasError: true, Next: next})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    s.cfg.AuthToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// safeNextPath rejects anything but an in-app absolute path, closing the
// open redirect an unvalidated scheme-relative ("//evil.example") or
// absolute URL target would otherwise hand the login form.
func safeNextPath(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}
