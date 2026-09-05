package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// rawPost issues a POST with full control over CSRF/auth headers so each
// security path can be probed in isolation.
func rawPost(t *testing.T, h http.Handler, path string, mut func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"name": {"owner/repo-1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if mut != nil {
		mut(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// CSRF: a mutation with no token/cookie is rejected before the service runs.
func TestCSRF_MissingTokenRejected(t *testing.T) {
	svc := &fakeService{}
	rec := rawPost(t, New(svc).Routes(), "/sessions/up", func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if svc.gotUp != nil {
		t.Error("Up must not run when CSRF fails")
	}
}

// CSRF: a token that does not match the cookie is rejected.
func TestCSRF_MismatchRejected(t *testing.T) {
	rec := rawPost(t, New(&fakeService{}).Routes(), "/sessions/up", func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
		r.Header.Set(csrfHeaderName, "a")
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "b"})
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// CSRF: a cross-origin request is rejected even with a matching token.
func TestCSRF_CrossOriginRejected(t *testing.T) {
	rec := rawPost(t, New(&fakeService{}).Routes(), "/sessions/up", func(r *http.Request) {
		r.Header.Set("Origin", "http://evil.example")
		r.Header.Set(csrfHeaderName, "tok")
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok"})
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// CSRF: a same-origin request with a matching token passes.
func TestCSRF_ValidPasses(t *testing.T) {
	svc := &fakeService{}
	rec := rawPost(t, New(svc).Routes(), "/sessions/up", func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
		r.Header.Set(csrfHeaderName, "tok")
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok"})
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if svc.gotUp == nil {
		t.Error("Up should have run for a valid request")
	}
}

// CSRF: a rejection under the JSON API namespace is JSON, not the HTML error
// banner every other route gets — the same "no HTML to a JSON consumer"
// contract the auth failure path holds.
func TestCSRF_APIPathRejectionIsJSON(t *testing.T) {
	rec := rawPost(t, New(&fakeService{}).Routes(), "/api/v1/bootstrap", func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// CSRF: GET pages mint a cookie and embed the token for htmx to echo back.
func TestCSRF_GetMintsCookieAndToken(t *testing.T) {
	rec := get(t, &fakeService{}, "/")
	var token string
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no CSRF cookie set on GET /")
	}
	if !strings.Contains(rec.Body.String(), token) {
		t.Error("CSRF token not embedded in the page (hx-headers)")
	}
}

// authServer wraps a fake behind a configured auth token.
func authServer() http.Handler {
	return NewWithConfig(&fakeService{status: sampleShow()}, &Config{AuthToken: "secret"}).Routes()
}

// Auth: an unauthenticated browser navigation is redirected to /login,
// carrying where it came from so a successful login returns there.
func TestAuth_RedirectsToLogin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login?next=%2F" {
		t.Errorf("Location = %q, want /login?next=%%2F", loc)
	}
}

// Auth: the redirect's next target is the page actually requested, not just
// the app root — the new React shell's entry included.
func TestAuth_RedirectsToLoginWithRequestedNext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/app/", nil)
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if loc := w.Header().Get("Location"); loc != "/login?next=%2Fapp%2F" {
		t.Errorf("Location = %q, want /login?next=%%2Fapp%%2F", loc)
	}
}

// Auth: a request under the JSON API namespace gets a JSON 401, never the
// HTML redirect a browser navigation gets — a JSON client following a
// redirect to /login would otherwise parse a sign-in page as its response.
func TestAuth_APIPathGetsJSONNotRedirect(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap", nil)
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q, want no redirect", loc)
	}
}

// Auth: healthz stays reachable without a token (liveness probes).
func TestAuth_HealthzExempt(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// Auth: the right token at /login sets the auth cookie and redirects in.
func TestAuth_LoginSetsCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"token": {"secret"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	var ok bool
	for _, c := range w.Result().Cookies() {
		if c.Name == authCookieName && c.Value == "secret" {
			ok = true
		}
	}
	if !ok {
		t.Error("auth cookie not set on successful login")
	}
}

// Auth: a successful login returns to the caller-supplied next path (e.g.
// where authMiddleware's redirect sent them from), not always the list.
func TestAuth_LoginRedirectsToNext(t *testing.T) {
	body := url.Values{"token": {"secret"}, "next": {"/app/"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if loc := w.Header().Get("Location"); loc != "/app/" {
		t.Errorf("Location = %q, want /app/", loc)
	}
}

// Auth: an off-app next value (open-redirect bait) falls back to "/" rather
// than being trusted.
func TestAuth_LoginRejectsUnsafeNext(t *testing.T) {
	for _, next := range []string{"//evil.example", "http://evil.example", "/\\evil.example"} {
		body := url.Values{"token": {"secret"}, "next": {next}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		authServer().ServeHTTP(w, req)
		if loc := w.Header().Get("Location"); loc != "/" {
			t.Errorf("next=%q: Location = %q, want /", next, loc)
		}
	}
}

// Auth: a wrong token re-renders the form with a 401.
func TestAuth_LoginWrongToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"token": {"nope"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// Auth: a valid cookie grants access to a protected page.
func TestAuth_CookieGrantsAccess(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/sessions/owner/repo-7", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "secret"})
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// Auth: a Bearer token grants access for non-browser callers.
func TestAuth_BearerGrantsAccess(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/sessions/owner/repo-7", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	authServer().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// Auth: an unauthenticated POST is a 401 (rejected before CSRF even runs).
func TestAuth_PostRequiresAuth(t *testing.T) {
	rec := rawPost(t, authServer(), "/sessions/up", func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
		r.Header.Set(csrfHeaderName, "tok")
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok"})
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
