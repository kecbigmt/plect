package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtWebApp stands in for a real Vite build, so these tests don't depend
// on whatever happens to be embedded in webapp.FS on disk.
func builtWebApp() fstest.MapFS {
	return fstest.MapFS{
		"static/unbuilt.html": &fstest.MapFile{Data: []byte("unused when built")},
		"static/dist/index.html": &fstest.MapFile{Data: []byte(
			`<html><body><div id="root"></div><script src="/app/assets/app.js"></script></body></html>`,
		)},
		"static/dist/assets/app.js": &fstest.MapFile{Data: []byte("// app\n")},
	}
}

// notBuiltWebApp mirrors what's actually committed to Git: static/dist/
// (Vite's gitignored output) doesn't exist at all.
func notBuiltWebApp() fstest.MapFS {
	return fstest.MapFS{"static/unbuilt.html": &fstest.MapFile{Data: []byte("Web UI not built notice")}}
}

func getWebApp(t *testing.T, root fs.FS, path string) *httptest.ResponseRecorder {
	t.Helper()
	srv := NewWithConfig(&fakeService{}, &Config{})
	srv.webAppFS = root
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestWebApp_ServesIndexAtRoot(t *testing.T) {
	rec := getWebApp(t, builtWebApp(), "/app/")
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

func TestWebApp_ServesBuiltAssets(t *testing.T) {
	root := builtWebApp()
	index := getWebApp(t, root, "/app/").Body.String()
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

	rec := getWebApp(t, root, assetPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", assetPath, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a JavaScript type", ct)
	}
}

func TestWebApp_UnknownPathFallsBackToIndex(t *testing.T) {
	rec := getWebApp(t, builtWebApp(), "/app/sessions/some-session")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Error("fallback response is not the shell's index.html")
	}
}

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

// static/dist/ (Vite's output) not existing at all — no index.html — is
// what a plain source build actually has (see embed.go); every /app/ route
// must fall back the same way.
func TestWebApp_ServesNotBuiltNoticeWhenDistHasNoIndex(t *testing.T) {
	for _, path := range []string{"/app/", "/app/sessions/some-session", "/app/assets/app.js"} {
		rec := getWebApp(t, notBuiltWebApp(), path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s: status = %d, want 503", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Web UI not built") {
			t.Errorf("GET %s: body = %q, want a not-built notice", path, rec.Body.String())
		}
	}
}

func TestWebApp_NotBuiltDoesNotAffectOtherRoutes(t *testing.T) {
	rec := getWebApp(t, notBuiltWebApp(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: status = %d, want 200", rec.Code)
	}
}
