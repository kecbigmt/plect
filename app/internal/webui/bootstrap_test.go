package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Bootstrap: an unauthenticated caller never reaches the handler, so it gets
// the shared JSON auth error, not a minted token.
func TestBootstrap_RequiresAuthWhenConfigured(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap", nil)
	authServer().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if _, ok := body["csrfToken"]; ok {
		t.Error("csrfToken must not be present in an unauthenticated response")
	}
}

// Bootstrap: an authenticated caller gets the API version and a CSRF token,
// and nothing else.
func TestBootstrap_ReturnsVersionAndCSRFTokenWhenAuthorized(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "secret"})
	authServer().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body["apiVersion"] != "v1" {
		t.Errorf("apiVersion = %v, want v1", body["apiVersion"])
	}
	if tok, _ := body["csrfToken"].(string); tok == "" {
		t.Error("csrfToken missing or empty")
	}
	if len(body) != 2 {
		t.Errorf("response has %d fields, want exactly apiVersion and csrfToken: %v", len(body), body)
	}

	var cookied bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName && c.Value == body["csrfToken"] {
			cookied = true
		}
	}
	if !cookied {
		t.Error("plect_csrf cookie not set to the returned token")
	}
}

// Bootstrap: no plugin-agnostic control here ever hands a cross-origin page
// permission to read the response — verified rather than assumed, since a
// browser blocks reading a cross-origin fetch's body only in the absence of
// a permissive Access-Control-Allow-Origin header.
func TestBootstrap_NoCORSHeaderForCrossOriginRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "secret"})
	req.Header.Set("Origin", "http://evil.example")
	authServer().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if h := rec.Header().Get("Access-Control-Allow-Origin"); h != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want unset", h)
	}
}

// Bootstrap: with no auth token configured (tailnet trust), the endpoint is
// reachable without any credential.
func TestBootstrap_NoAuthConfiguredIsReachable(t *testing.T) {
	rec := get(t, &fakeService{}, "/api/v1/bootstrap")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
