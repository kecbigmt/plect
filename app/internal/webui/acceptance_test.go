//go:build integration

package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	webapiv1 "github.com/kecbigmt/plecture/app/internal/webapi/generated"
)

// mountResolverOnlyWorkspaceProvider registers a minimal global-layer workflow +
// workspace provider so dispatch can resolve a github.com URL without depending on
// whatever catalogs.toml (if any) happens to be registered on the machine
// the test runs on. The workspace provider's setup hook is a placeholder that must
// never actually run — Create's allowlist check is meant to reject the
// request before any workspace provider hook does.
func mountResolverOnlyWorkspaceProvider(t *testing.T, cfg *config.Config) {
	t.Helper()
	base := t.TempDir()
	cfg.BaseDir = base
	// Clearing the mounted plugins is what the independence above actually
	// requires: overriding BaseDir alone leaves whatever catalog this machine
	// has mounted in the load, so the test would fail or pass on the strength
	// of config it does not own.
	cfg.PluginDirs = nil
	cfg.Plugins = nil
	if err := os.MkdirAll(filepath.Join(base, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	workflowTOML := "[test]\nkind = \"workflow\"\nworkspace_provider = \"test_gh\"\n"
	if err := os.WriteFile(filepath.Join(base, "workflows", "test.toml"), []byte(workflowTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceProviderTOML := "[test_gh]\n" +
		"kind  = \"workspace_provider\"\n" +
		"match = '^https://github\\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(?:issues|pull)/(?P<number>\\d+)'\n" +
		"name  = { expr = \"match.owner + '/' + match.repo + '-' + match.number\" }\n" +
		// Never invoked (the allowlist check rejects the request first) —
		// only present because a provider must declare how it acquires.
		"\n[test_gh.setup]\ntype = \"exec\"\ncommand = \"false\"\n"
	if err := os.WriteFile(filepath.Join(base, "workspaces", "test_gh.toml"), []byte(workspaceProviderTOML), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolateMachineConfig strips the layers this machine owns, so a case that
// loads a real config still answers only for what it declares itself.
func isolateMachineConfig(cfg *config.Config) {
	cfg.BaseDir = ""
	cfg.PluginDirs = nil
	cfg.Plugins = nil
}

// Acceptance: the real service stack (state.Store + service.List), driven
// through the HTTP handler, surfaces a session that exists in state.json.
//
// Given a session persisted in a temp state store,
// When GET / is served by the live service,
// Then the response lists that session.
func TestAcceptance_SessionAppearsInList(t *testing.T) {
	store := state.NewStore(t.TempDir())
	sess := &domain.Session{
		Name:             "acceptance/web-1",
		ResourceID:       "https://github.com/acceptance/web/issues/1",
		Branch:           "issue/1",
		WorkspaceDirPath: "/nonexistent/workspace-dir",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := store.Put(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc := newLiveService(cfg, store)
	rec := get(t, svc, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "acceptance/web-1") {
		t.Errorf("seeded session not listed; body:\n%s", body)
	}
}

// Acceptance: the generated-contract JSON API is reachable through the same
// mux, at its documented version prefix, over the real service stack — not
// just through the fake SessionReader webapi's own package tests use.
//
// Given a session persisted in a temp state store,
// When GET /api/v1/sessions/<name> is served by the live service,
// Then the response is the SessionDetail JSON for that session.
func TestAcceptance_ApiV1SessionDetailServesSeededSession(t *testing.T) {
	store := state.NewStore(t.TempDir())
	sess := &domain.Session{
		Name:             "acceptance/web-2",
		ResourceID:       "https://github.com/acceptance/web/issues/2",
		Branch:           "issue/2",
		WorkspaceDirPath: "/nonexistent/workspace-dir",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := store.Put(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc := newLiveService(cfg, store)
	rec := get(t, svc, "/api/v1/sessions/acceptance/web-2")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var detail webapiv1.SessionDetail
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("decode SessionDetail: %v", err)
	}
	if detail.SessionName != "acceptance/web-2" {
		t.Errorf("SessionName = %q, want acceptance/web-2", detail.SessionName)
	}
	if detail.Branch == nil || *detail.Branch != "issue/2" {
		t.Errorf("Branch = %v, want issue/2", detail.Branch)
	}
}

// Given no session by that name exists,
// When GET /api/v1/sessions/<name> is served,
// Then the response is a 404 NotFoundError, not a 500 or an HTML error page.
func TestAcceptance_ApiV1SessionDetailUnknownNameIs404(t *testing.T) {
	store := state.NewStore(t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc := newLiveService(cfg, store)
	rec := get(t, svc, "/api/v1/sessions/acceptance/missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body)
	}
	var apiErr webapiv1.NotFoundError
	if err := json.NewDecoder(rec.Body).Decode(&apiErr); err != nil {
		t.Fatalf("decode NotFoundError: %v", err)
	}
	if apiErr.Code != webapiv1.SessionNotFound {
		t.Errorf("Code = %q, want session_not_found", apiErr.Code)
	}
}

func TestAcceptance_ApiV1SessionsPreserveParentChildGrandchildAndIndependentRoots(t *testing.T) {
	store := state.NewStore(t.TempDir())
	now := time.Now()
	for _, n := range []string{"acceptance/root-a", "acceptance/child-b", "acceptance/grandchild-c", "acceptance/root-d"} {
		if err := store.Put(&domain.Session{Name: n, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	for child, parent := range map[string]string{
		"acceptance/child-b":      "acceptance/root-a",
		"acceptance/grandchild-c": "acceptance/child-b",
	} {
		if err := store.Update(child, func(s *domain.Session) error {
			s.ParentSession = parent
			return nil
		}); err != nil {
			t.Fatalf("set parent for %s: %v", child, err)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	isolateMachineConfig(cfg)
	svc := newLiveService(cfg, store)

	listRec := get(t, svc, "/api/v1/sessions")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body: %s", listRec.Code, listRec.Body)
	}
	var list webapiv1.SessionListResponse
	if err := json.NewDecoder(listRec.Body).Decode(&list); err != nil {
		t.Fatalf("decode SessionListResponse: %v", err)
	}
	wantParents := map[string]*string{
		"acceptance/root-a":       nil,
		"acceptance/child-b":      strPtr("acceptance/root-a"),
		"acceptance/grandchild-c": strPtr("acceptance/child-b"),
		"acceptance/root-d":       nil,
	}
	byName := make(map[string]webapiv1.SessionSummary, len(list.Items))
	for _, item := range list.Items {
		byName[item.SessionName] = item
	}
	for name, want := range wantParents {
		item, ok := byName[name]
		if !ok {
			t.Errorf("list did not include %s", name)
			continue
		}
		switch {
		case want == nil && item.ParentSession != nil:
			t.Errorf("%s list parentSession = %v, want absent", name, *item.ParentSession)
		case want != nil && (item.ParentSession == nil || *item.ParentSession != *want):
			t.Errorf("%s list parentSession = %v, want %s", name, item.ParentSession, *want)
		}
	}

	detail := func(name string) webapiv1.SessionDetail {
		t.Helper()
		rec := get(t, svc, "/api/v1/sessions/"+name)
		if rec.Code != http.StatusOK {
			t.Fatalf("detail(%s) status = %d, want 200; body: %s", name, rec.Code, rec.Body)
		}
		var d webapiv1.SessionDetail
		if err := json.NewDecoder(rec.Body).Decode(&d); err != nil {
			t.Fatalf("decode SessionDetail(%s): %v", name, err)
		}
		return d
	}

	root := detail("acceptance/root-a")
	if root.Children == nil || len(*root.Children) != 1 || (*root.Children)[0] != "acceptance/child-b" {
		t.Errorf("root-a children = %v, want [acceptance/child-b] (grandchild must not flatten in)", root.Children)
	}

	grandchild := detail("acceptance/grandchild-c")
	if grandchild.ParentSession == nil || *grandchild.ParentSession != "acceptance/child-b" {
		t.Errorf("grandchild-c parentSession = %v, want acceptance/child-b", grandchild.ParentSession)
	}

	independentRoot := detail("acceptance/root-d")
	if independentRoot.ParentSession != nil {
		t.Errorf("root-d parentSession = %v, want absent", independentRoot.ParentSession)
	}
	if independentRoot.Children != nil {
		t.Errorf("root-d children = %v, want absent", independentRoot.Children)
	}
}

func strPtr(s string) *string { return &s }

// Acceptance: the real service stack, driven through the HTTP handler, renders
// the detail page for a session that exists in state.json.
//
// Given a session persisted in a temp state store,
// When GET /sessions/<name> is served by the live service,
// Then the response shows that session's branch and a diagnostic for the
// missing workspace directory (the seeded path does not exist).
func TestAcceptance_SessionDetail(t *testing.T) {
	store := state.NewStore(t.TempDir())
	sess := &domain.Session{
		Name:             "acceptance/web-2",
		ResourceID:       "https://github.com/acceptance/web/issues/2",
		Branch:           "issue/2",
		WorkspaceDirPath: "/nonexistent/workspace-dir",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := store.Put(sess); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// An acceptance case answers for this stack, not for whatever config the
	// machine running it happens to own: a session's detail view loads the
	// declarations its workflow names, so leaving the machine's own layers in
	// the load would make this pass or fail on their strength.
	isolateMachineConfig(cfg)
	svc := newLiveService(cfg, store)
	rec := get(t, svc, "/sessions/acceptance/web-2")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "acceptance/web-2") || !strings.Contains(body, "issue/2") {
		t.Errorf("detail missing session fields; body:\n%s", body)
	}
	if !strings.Contains(body, "(missing)") {
		t.Errorf("detail should surface the missing-workspace-dir diagnostic; body:\n%s", body)
	}
}

// Acceptance: an unknown session name returns 404 through the real stack.
func TestAcceptance_SessionDetailNotFound(t *testing.T) {
	store := state.NewStore(t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	svc := newLiveService(cfg, store)
	rec := get(t, svc, "/sessions/acceptance/missing-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// acceptancePost drives a same-origin htmx POST through a handler built from the
// real service stack.
func acceptancePost(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.Header.Set(csrfHeaderName, "tok")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "tok"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Acceptance: a create for a resource outside the allowlist is a 403 through
// the real stack — the allowlist check fires before any workspace provider work.
func TestAcceptance_CreateResourceNotAllowed(t *testing.T) {
	store := state.NewStore(t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	mountResolverOnlyWorkspaceProvider(t, cfg)
	cfg.ResourceAllowlist = []string{`^https://github\.com/only/allowed/`}

	h := New(newLiveService(cfg, store)).Routes()
	rec := acceptancePost(t, h, "/sessions", url.Values{
		"url": {"https://github.com/other/repo/issues/1"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body:\n%s", rec.Code, rec.Body.String())
	}
}
