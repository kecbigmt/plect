package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// WebApp: the root path serves the shell's index.html.
func TestWebApp_ServesIndexAtRoot(t *testing.T) {
	rec := get(t, &fakeService{}, "/app/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Error(`body does not contain the shell's #root mount point`)
	}
}

// WebApp: a built asset (referenced by index.html's own script/link tags) is
// served from the embedded build.
func TestWebApp_ServesBuiltAssets(t *testing.T) {
	index := get(t, &fakeService{}, "/app/").Body.String()
	start := strings.Index(index, `src="/app/assets/`)
	if start == -1 {
		t.Fatal("index.html has no /app/assets/ script reference to follow")
	}
	start += len(`src="`)
	end := strings.Index(index[start:], `"`)
	if end == -1 {
		t.Fatal("unterminated src attribute in index.html")
	}
	assetPath := index[start : start+end]

	rec := get(t, &fakeService{}, assetPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", assetPath, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a JavaScript type", ct)
	}
}

// WebApp: a path the built assets don't recognize falls back to index.html,
// so a direct navigation/reload of a future client-side route still loads
// the shell instead of a bare 404.
func TestWebApp_UnknownPathFallsBackToIndex(t *testing.T) {
	rec := get(t, &fakeService{}, "/app/sessions/some-session")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Error("fallback response is not the shell's index.html")
	}
}

// WebApp: it sits behind the same auth gate as every other route, carrying
// its own path as the login redirect's next target.
func TestWebApp_RequiresAuthWhenConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/app/", nil)
	rec := httptest.NewRecorder()
	authServer().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login?next=%2Fapp%2F" {
		t.Errorf("Location = %q, want /login?next=%%2Fapp%%2F", loc)
	}
}
