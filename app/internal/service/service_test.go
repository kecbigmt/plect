package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/confighome"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// PLECT_CONFIG_HOME and XDG_CONFIG_HOME both outrank HOME in
// confighome.Resolve()'s precedence, so left ambient either would bypass
// every test's HOME-based isolation below; PLECT_SESSION_NAME is unset
// because this repo's own dev loop sets it in a plect pane.
func TestMain(m *testing.M) {
	os.Unsetenv("PLECT_SESSION_NAME")
	os.Unsetenv(confighome.EnvVar)
	os.Unsetenv(confighome.XDGEnvVar)
	os.Exit(m.Run())
}

func testStore(t *testing.T) *state.Store {
	t.Helper()
	return state.NewStore(t.TempDir())
}

// List ranges store.All() (a map), so without sorting its order is random and
// the web UI's auto-refresh reshuffles. Sessions must come back sorted by name.
func TestList_SortsTrackedByName(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	for _, n := range []string{"zzz/web-3", "aaa/web-1", "mmm/web-2"} {
		if err := store.Put(&domain.Session{Name: n, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	entries, err := List(&config.Config{}, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var got []string
	for _, e := range entries {
		if e.SessionName == "aaa/web-1" || e.SessionName == "mmm/web-2" || e.SessionName == "zzz/web-3" {
			got = append(got, e.SessionName)
		}
	}
	want := []string{"aaa/web-1", "mmm/web-2", "zzz/web-3"}
	if !slices.Equal(got, want) {
		t.Errorf("tracked order = %v, want %v", got, want)
	}
}

func findEntry(t *testing.T, entries []ListEntry, name string) ListEntry {
	t.Helper()
	for _, e := range entries {
		if e.SessionName == name {
			return e
		}
	}
	t.Fatalf("%q missing from List results", name)
	return ListEntry{}
}

// probeCallCount treats a missing marker file as zero calls, not an error.
func probeCallCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read probe marker %q: %v", path, err)
	}
	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

func TestList_ReadsPersistedHealthWithoutProbing(t *testing.T) {
	store := testStore(t)
	marker := filepath.Join(t.TempDir(), "probe-calls")
	cfg := aliveFixtureConfig(t, fmt.Sprintf("echo hit >> %s", marker))
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})

	// Simulate the reactor's periodic sweep (service.HealthcheckSession),
	// which runs independently of any listing.
	if _, err := EvaluateHealth(cfg, store, "owner/repo-1"); err != nil {
		t.Fatalf("EvaluateHealth (sweep): %v", err)
	}
	if got := probeCallCount(t, marker); got != 1 {
		t.Fatalf("probe calls after the sweep = %d, want 1", got)
	}

	for i := range 3 {
		entries, err := List(cfg, store)
		if err != nil {
			t.Fatalf("List call %d: %v", i, err)
		}
		if got := findEntry(t, entries, "owner/repo-1").Health; got != domain.HealthHealthy {
			t.Errorf("List call %d: Health = %q, want healthy (the sweep's verdict)", i, got)
		}
	}

	if got := probeCallCount(t, marker); got != 1 {
		t.Errorf("probe calls after 3 List calls = %d, want 1 — List must never probe", got)
	}
}

func TestList_NeverSweptSessionHasNoHealth(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "true")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})

	entries, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	entry := findEntry(t, entries, "owner/repo-1")
	if entry.Health != domain.HealthState("") {
		t.Errorf("Health = %q, want absent (never swept)", entry.Health)
	}

	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), `"health"`) {
		t.Errorf("JSON = %s, want the health field omitted for a never-swept session", b)
	}
}

func TestList_SurfacesSweptUnhealthyReason(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "false")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	if _, err := EvaluateHealth(cfg, store, "owner/repo-1"); err != nil {
		t.Fatalf("EvaluateHealth (sweep): %v", err)
	}

	entries, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	entry := findEntry(t, entries, "owner/repo-1")
	if entry.Health != domain.HealthUnhealthy {
		t.Fatalf("Health = %q, want unhealthy", entry.Health)
	}
	sess, err := store.GetE("owner/repo-1")
	if err != nil {
		t.Fatalf("GetE: %v", err)
	}
	if sess.Health == nil || entry.HealthReason != sess.Health.LastReason || entry.HealthReason == "" {
		t.Errorf("HealthReason = %q, want the sweep's persisted reason %+v", entry.HealthReason, sess.Health)
	}
}

// The sweep skips a down session, so its last recorded verdict can predate
// the shutdown by any amount.
func TestList_DownSessionMasksStalePersistedHealth(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "true")
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	if _, err := EvaluateHealth(cfg, store, "owner/repo-1"); err != nil {
		t.Fatalf("EvaluateHealth (sweep while up): %v", err)
	}
	// Bring the session down without a further sweep, mirroring the reactor,
	// which skips a down session's healthcheck entirely.
	if err := store.Update("owner/repo-1", func(s *domain.Session) error {
		s.Tasks["initial"].Status = contract.TaskStatusCleaned
		return nil
	}); err != nil {
		t.Fatalf("bring down: %v", err)
	}

	entries, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	entry := findEntry(t, entries, "owner/repo-1")
	if entry.Run != domain.RunDown {
		t.Fatalf("Run = %q, want down (test setup)", entry.Run)
	}
	if entry.Health != domain.HealthState("") {
		t.Errorf("Health = %q, want absent — a down session has no verdict, even with a stale prior sweep", entry.Health)
	}
	if entry.HealthReason != "" {
		t.Errorf("HealthReason = %q, want empty", entry.HealthReason)
	}
}

// A workflow that never resolves never completes a sweep either.
func TestList_GhostWorkflowSessionHasNoHealthButOthersListNormally(t *testing.T) {
	store := testStore(t)
	cfg := currentPlanConfig(t, "true", "true") // only declares workflow "default"
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "ghost-workflow", map[string]*contract.TaskState{
		"pane": {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
	})
	seedSession(t, store, "owner/repo-2", "owner/repo", 2, "default", map[string]*contract.TaskState{
		"pane":  {Scope: contract.TaskScopeRun, TaskID: "pane", Status: contract.TaskStatusProduced},
		"agent": {Scope: contract.TaskScopeRun, TaskID: "agent", Status: contract.TaskStatusProduced},
	})
	if _, err := EvaluateHealth(cfg, store, "owner/repo-2"); err != nil {
		t.Fatalf("EvaluateHealth (sweep): %v", err)
	}

	entries, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	broken := findEntry(t, entries, "owner/repo-1")
	if broken.Health != domain.HealthState("") {
		t.Errorf("never-swept session Health = %q, want absent", broken.Health)
	}
	healthy := findEntry(t, entries, "owner/repo-2")
	if healthy.Health != domain.HealthHealthy {
		t.Errorf("swept session Health = %q, want healthy — one never-swept session must not affect it", healthy.Health)
	}
}

func TestResolveSession_ByURL(t *testing.T) {
	store := testStore(t)
	// No session in store → should fail with session_not_found
	_, _, err := resolveSession(nil, store, "https://github.com/org/repo/issues/1")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrSessionNotFound {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrSessionNotFound)
	}
}

func TestResolveSession_BySessionName(t *testing.T) {
	store := testStore(t)
	_, _, err := resolveSession(nil, store, "org/repo-1")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrSessionNotFound {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrSessionNotFound)
	}
}

func TestResolveSession_UnknownIdentifier(t *testing.T) {
	store := testStore(t)
	_, _, err := resolveSession(nil, store, "https://example.test/org/repo")
	if err == nil {
		t.Fatal("expected error for an identifier with no state entry")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrSessionNotFound {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrSessionNotFound)
	}
}

func TestResolveSessionName_BySessionName(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	if err := store.Put(&domain.Session{Name: "org/repo-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	name, err := ResolveSessionName(&config.Config{}, store, "org/repo-1")
	if err != nil {
		t.Fatalf("ResolveSessionName: %v", err)
	}
	if name != "org/repo-1" {
		t.Errorf("name = %q, want %q", name, "org/repo-1")
	}
}

func TestResolveSessionName_UnknownIdentifier(t *testing.T) {
	store := testStore(t)
	_, err := ResolveSessionName(&config.Config{}, store, "org/missing")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrSessionNotFound {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrSessionNotFound)
	}
}

func TestValidTag(t *testing.T) {
	tests := []struct {
		tag   string
		valid bool
	}{
		{"review", true},
		{"debug-1", true},
		{"my_tag", true},
		{"ABC123", true},
		{"", true},     // empty tag is allowed (means no tag)
		{"a+b", false}, // + is the separator
		{"a/b", false}, // path separator
		{"a b", false}, // space
		{"a:b", false}, // tmux conflict
		{"日本語", false}, // multibyte sample: exercises rejection of non-ASCII tags
	}
	for _, tt := range tests {
		if tt.tag == "" {
			continue // empty tag skips validation
		}
		got := validTag.MatchString(tt.tag)
		if got != tt.valid {
			t.Errorf("validTag.MatchString(%q) = %v, want %v", tt.tag, got, tt.valid)
		}
	}
}

func TestStatus_UnknownIdentifier(t *testing.T) {
	cfg := &config.Config{}
	store := testStore(t)
	_, err := Status(cfg, store, "https://example.test/org/repo")
	if err == nil {
		t.Fatal("expected error for an identifier with no state entry")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrSessionNotFound {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrSessionNotFound)
	}
}

func TestStatus_SessionNameNotFound(t *testing.T) {
	cfg := &config.Config{}
	store := testStore(t)
	_, err := Status(cfg, store, "owner/repo-123")
	if err == nil {
		t.Fatal("expected error for unknown session name")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrSessionNotFound {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrSessionNotFound)
	}
}

func TestStatus_DestroyedSessionReturnsTombstone(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: "true", cleanup: "true"},
		},
		[]nodeFixture{{id: "envfile"}},
	)
	sessionName := "org/repo-1"
	seedSession(t, store, sessionName, "org/repo", 1, "default", map[string]*contract.TaskState{
		"envfile": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"path": "/tmp/env"},
		},
	})
	if _, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	result, err := Status(cfg, store, sessionName)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !result.Destroyed {
		t.Fatal("expected Destroyed = true")
	}
	if result.DestroyedAt.IsZero() {
		t.Error("expected DestroyedAt to be set")
	}
	if result.Identity.SessionName != sessionName {
		t.Errorf("SessionName = %q, want %q", result.Identity.SessionName, sessionName)
	}
	found := false
	for _, tv := range result.Work {
		if tv.Instance == "envfile" && tv.Outputs["path"] == "/tmp/env" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected envfile task outputs preserved in tombstone-backed Status, got %+v", result.Work)
	}
}

func TestStatus_DestroyedSessionWithoutTombstoneStillErrors(t *testing.T) {
	cfg := &config.Config{}
	store := testStore(t)
	_, err := Status(cfg, store, "owner/repo-999")
	if err == nil {
		t.Fatal("expected error for unknown session with no tombstone")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

// Status projects the session tree (parent + children, derived from the parent
// pointers Subtree walks), so the web detail can link across the tree.
func TestStatus_ProjectsTree(t *testing.T) {
	cfg := &config.Config{}
	store := testStore(t)
	now := time.Now()
	wt := t.TempDir()
	for _, n := range []string{"org/repo-1", "org/repo-2", "org/repo-3"} {
		if err := store.Put(&domain.Session{
			Name: n, CreatedAt: now, UpdatedAt: now,
			WorkspaceDirPath: wt,
		}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	setParent(t, store, "org/repo-2", "org/repo-1")
	setParent(t, store, "org/repo-3", "org/repo-1")

	root, err := Status(cfg, store, "org/repo-1")
	if err != nil {
		t.Fatalf("Status(root): %v", err)
	}
	if root.Identity.ParentSession != "" {
		t.Errorf("root ParentSession = %q, want empty", root.Identity.ParentSession)
	}
	if want := []string{"org/repo-2", "org/repo-3"}; !slices.Equal(root.Identity.Children, want) {
		t.Errorf("root Children = %v, want %v", root.Identity.Children, want)
	}

	child, err := Status(cfg, store, "org/repo-2")
	if err != nil {
		t.Fatalf("Status(child): %v", err)
	}
	if child.Identity.ParentSession != "org/repo-1" {
		t.Errorf("child ParentSession = %q, want org/repo-1", child.Identity.ParentSession)
	}
	if len(child.Identity.Children) != 0 {
		t.Errorf("leaf Children = %v, want none", child.Identity.Children)
	}
}

func TestStatus_ProjectsTree_GrandchildAndIndependentRootStayDistinct(t *testing.T) {
	cfg := &config.Config{}
	store := testStore(t)
	now := time.Now()
	for _, n := range []string{"org/root-a", "org/child-b", "org/grandchild-c", "org/root-d"} {
		if err := store.Put(&domain.Session{Name: n, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	setParent(t, store, "org/child-b", "org/root-a")
	setParent(t, store, "org/grandchild-c", "org/child-b")

	entries, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := make(map[string]ListEntry, len(entries))
	for _, e := range entries {
		byName[e.SessionName] = e
	}
	if got := byName["org/root-a"].ParentSession; got != "" {
		t.Errorf("root-a ParentSession = %q, want empty", got)
	}
	if got := byName["org/root-d"].ParentSession; got != "" {
		t.Errorf("independent root-d ParentSession = %q, want empty", got)
	}

	root, err := Status(cfg, store, "org/root-a")
	if err != nil {
		t.Fatalf("Status(root-a): %v", err)
	}
	if want := []string{"org/child-b"}; !slices.Equal(root.Identity.Children, want) {
		t.Errorf("root-a Children = %v, want %v (grandchild must not flatten in)", root.Identity.Children, want)
	}

	grandchild, err := Status(cfg, store, "org/grandchild-c")
	if err != nil {
		t.Fatalf("Status(grandchild-c): %v", err)
	}
	if grandchild.Identity.ParentSession != "org/child-b" {
		t.Errorf("grandchild-c ParentSession = %q, want org/child-b", grandchild.Identity.ParentSession)
	}

	independentRoot, err := Status(cfg, store, "org/root-d")
	if err != nil {
		t.Fatalf("Status(root-d): %v", err)
	}
	if independentRoot.Identity.ParentSession != "" {
		t.Errorf("root-d ParentSession = %q, want empty", independentRoot.Identity.ParentSession)
	}
	if len(independentRoot.Identity.Children) != 0 {
		t.Errorf("root-d Children = %v, want none", independentRoot.Identity.Children)
	}
}

// A "root:<name>" pseudo-parent (resolveParentSession's opt-in explicit
// sibling group) is not a real, selectable session, so it must not gain a
// Children entry the way a real parent does.
func TestStatus_ProjectsTree_ExplicitRootGroupSiblings(t *testing.T) {
	cfg := &config.Config{}
	store := testStore(t)
	now := time.Now()
	if err := store.Put(&domain.Session{Name: "org/group-anchor", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed anchor: %v", err)
	}
	for _, n := range []string{"org/sibling-x", "org/sibling-y"} {
		if err := store.Put(&domain.Session{
			Name: n, CreatedAt: now, UpdatedAt: now,
			ParentSession: "root:org/group-anchor",
		}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	const wantGroup = "root:org/group-anchor"
	entries, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, name := range []string{"org/sibling-x", "org/sibling-y"} {
		var found bool
		for _, e := range entries {
			if e.SessionName != name {
				continue
			}
			found = true
			if e.ParentSession != wantGroup {
				t.Errorf("%s ParentSession = %q, want %q", name, e.ParentSession, wantGroup)
			}
		}
		if !found {
			t.Fatalf("List did not include %s", name)
		}

		status, err := Status(cfg, store, name)
		if err != nil {
			t.Fatalf("Status(%s): %v", name, err)
		}
		if status.Identity.ParentSession != wantGroup {
			t.Errorf("Status(%s).ParentSession = %q, want %q", name, status.Identity.ParentSession, wantGroup)
		}
	}

	anchor, err := Status(cfg, store, "org/group-anchor")
	if err != nil {
		t.Fatalf("Status(anchor): %v", err)
	}
	if len(anchor.Identity.Children) != 0 {
		t.Errorf("anchor Children = %v, want none (siblings are not the anchor's children)", anchor.Identity.Children)
	}
}

// TestStatus_ProjectsTree_KeepsParentLinkAfterParentDestroyed proves destroy
// no longer orphans children the way a hard delete once did: the parent's
// row (and its name) is retained, so its child's ParentSession still names
// it, even though the parent itself no longer appears in a live listing.
func TestStatus_ProjectsTree_KeepsParentLinkAfterParentDestroyed(t *testing.T) {
	cfg := &config.Config{}
	store := testStore(t)
	now := time.Now()
	if err := store.Put(&domain.Session{Name: "org/parent-p", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if err := store.Put(&domain.Session{Name: "org/child-q", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	setParent(t, store, "org/child-q", "org/parent-p")

	if err := store.Destroy("org/parent-p"); err != nil {
		t.Fatalf("Destroy(parent): %v", err)
	}

	entries, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.SessionName != "org/child-q" {
			continue
		}
		found = true
		if e.ParentSession != "org/parent-p" {
			t.Errorf("child ParentSession = %q, want %q (a destroyed parent keeps its children's history intact)", e.ParentSession, "org/parent-p")
		}
	}
	if !found {
		t.Fatal("List no longer includes the child")
	}
	for _, e := range entries {
		if e.SessionName == "org/parent-p" {
			t.Error("List includes the destroyed parent, want it hidden by default")
		}
	}

	status, err := Status(cfg, store, "org/child-q")
	if err != nil {
		t.Fatalf("Status(child): %v", err)
	}
	if status.Identity.ParentSession != "org/parent-p" {
		t.Errorf("Status ParentSession = %q, want %q", status.Identity.ParentSession, "org/parent-p")
	}
}

func TestSetMessage(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	store.Put(&domain.Session{
		Name:      "owner/repo-1",
		CreatedAt: now,
		UpdatedAt: now,
	})

	if err := SetMessage(nil, store, "owner/repo-1", "working"); err != nil {
		t.Fatalf("SetMessage() error: %v", err)
	}

	got := LatestStatusMessage(store, "owner/repo-1")
	if got == nil {
		t.Fatal("Message should be set")
	}
	if got.Text != "working" {
		t.Errorf("Text = %q, want %q", got.Text, "working")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set")
	}

	// Empty text unsets the message rather than persisting a blank line.
	if err := SetMessage(nil, store, "owner/repo-1", ""); err != nil {
		t.Fatalf("SetMessage(\"\") error: %v", err)
	}
	if LatestStatusMessage(store, "owner/repo-1") != nil {
		t.Error("empty text should clear Message")
	}
}

func TestSetMessage_EmitsStatusMessageEventsOnlyWhenTextChanges(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	store.Put(&domain.Session{Name: "owner/repo-1", CreatedAt: now, UpdatedAt: now})

	if err := SetMessage(nil, store, "owner/repo-1", "working"); err != nil {
		t.Fatalf("SetMessage(working) error: %v", err)
	}
	assertStatusMessageEvents(t, store, "owner/repo-1", []event.Event{
		{
			Type:      event.TypeStatusMessage,
			Source:    event.SourcePlect,
			Direction: event.Outbound,
			Summary:   "working",
			Metadata:  map[string]string{"text": "working", "cleared": "false", "previous": ""},
		},
	})

	if err := SetMessage(nil, store, "owner/repo-1", "working"); err != nil {
		t.Fatalf("SetMessage(same) error: %v", err)
	}
	assertStatusMessageEvents(t, store, "owner/repo-1", []event.Event{
		{
			Type:      event.TypeStatusMessage,
			Source:    event.SourcePlect,
			Direction: event.Outbound,
			Summary:   "working",
			Metadata:  map[string]string{"text": "working", "cleared": "false", "previous": ""},
		},
	})

	if err := SetMessage(nil, store, "owner/repo-1", "working: Bash go"); err != nil {
		t.Fatalf("SetMessage(changed) error: %v", err)
	}
	if err := SetMessage(nil, store, "owner/repo-1", ""); err != nil {
		t.Fatalf("SetMessage(clear) error: %v", err)
	}
	if err := SetMessage(nil, store, "owner/repo-1", ""); err != nil {
		t.Fatalf("SetMessage(clear same) error: %v", err)
	}
	assertStatusMessageEvents(t, store, "owner/repo-1", []event.Event{
		{
			Type:      event.TypeStatusMessage,
			Source:    event.SourcePlect,
			Direction: event.Outbound,
			Summary:   "working",
			Metadata:  map[string]string{"text": "working", "cleared": "false", "previous": ""},
		},
		{
			Type:      event.TypeStatusMessage,
			Source:    event.SourcePlect,
			Direction: event.Outbound,
			Summary:   "working: Bash go",
			Metadata:  map[string]string{"text": "working: Bash go", "cleared": "false", "previous": "working"},
		},
		{
			Type:      event.TypeStatusMessage,
			Source:    event.SourcePlect,
			Direction: event.Outbound,
			Summary:   "",
			Metadata:  map[string]string{"text": "", "cleared": "true", "previous": "working: Bash go"},
		},
	})
}

func TestSetMessage_FirstExplicitEmptyReportEmitsClearEvent(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	store.Put(&domain.Session{Name: "owner/repo-1", CreatedAt: now, UpdatedAt: now})

	if err := SetMessage(nil, store, "owner/repo-1", ""); err != nil {
		t.Fatalf("SetMessage(empty) error: %v", err)
	}
	if err := SetMessage(nil, store, "owner/repo-1", ""); err != nil {
		t.Fatalf("SetMessage(repeated empty) error: %v", err)
	}

	assertStatusMessageEvents(t, store, "owner/repo-1", []event.Event{
		{
			Type:      event.TypeStatusMessage,
			Source:    event.SourcePlect,
			Direction: event.Outbound,
			Metadata:  map[string]string{"text": "", "cleared": "true", "previous": ""},
		},
	})
}

func assertStatusMessageEvents(t *testing.T, store *state.Store, sessionName string, want []event.Event) {
	t.Helper()
	got, _, _, err := eventlog.NewStore(store.Dir()).List(sessionName, 0, event.Filter{Types: []string{event.TypeStatusMessage}})
	if err != nil {
		t.Fatalf("List status messages: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("status message events = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type || got[i].Source != want[i].Source || got[i].Direction != want[i].Direction || got[i].Summary != want[i].Summary || got[i].Body != "" {
			t.Fatalf("event[%d] = %+v, want type/source/direction/summary/body from %+v", i, got[i], want[i])
		}
		for key, value := range want[i].Metadata {
			if got[i].Metadata[key] != value {
				t.Fatalf("event[%d].Metadata[%q] = %q, want %q; metadata=%+v", i, key, got[i].Metadata[key], value, got[i].Metadata)
			}
		}
	}
}

func TestSetMessage_SessionNotFound(t *testing.T) {
	store := testStore(t)
	err := SetMessage(nil, store, "owner/repo-999", "working")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrSessionNotFound {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrSessionNotFound)
	}
}

// SetMessage is a per-session write path (hook scripts call it on every turn
// boundary), so it must honor SessionGuard like Create/Destroy/EventPublish —
// otherwise a guarded orchestrator could still relabel another owner's
// session's status.
func TestSetMessage_SessionGuardBlocksCrossOwner(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	store.Put(&domain.Session{Name: "exampleorg/repo-26", CreatedAt: now, UpdatedAt: now})
	cfg := &config.Config{SessionGuard: "^acme/"}

	err := SetMessage(cfg, store, "exampleorg/repo-26", "working")
	if err == nil {
		t.Fatal("expected session-guard rejection for cross-owner message write")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRepoNotAllowed {
		t.Errorf("want ErrRepoNotAllowed, got %v", err)
	}
	if LatestStatusMessage(store, "exampleorg/repo-26") != nil {
		t.Error("blocked SetMessage must not append a status_message event")
	}
}

func TestApplyDisplay_OverridesFromOutputs(t *testing.T) {
	workflows := map[string]config.WorkflowFile{
		"wf": {
			ID: "wf",
			Display: map[string]*lang.Value{
				"title":  fromValue("workflow.outputs.title"),
				"status": fromValue("workflow.outputs.pr_state"),
			},
		},
	}
	s := &domain.Session{
		Name:     "org/repo-1",
		Workflow: "wf",
		Tasks: map[string]*contract.TaskState{
			contract.WorkflowPseudoNodeID: {
				Status:  contract.TaskStatusProduced,
				Outputs: map[string]any{"title": "Fix the bug", "pr_state": "open"},
			},
		},
	}
	cached := cachedInfo{Title: "from-cache", DisplayStatus: "cache-status"}
	applyDisplay(workflows, s, &cached)
	if cached.Title != "Fix the bug" {
		t.Errorf("Title = %q, want display override", cached.Title)
	}
	if cached.DisplayStatus != "open" {
		t.Errorf("GitHubStatus = %q, want open", cached.DisplayStatus)
	}
}

func TestApplyDisplay_EmptyRenderKeepsFallback(t *testing.T) {
	workflows := map[string]config.WorkflowFile{
		"wf": {ID: "wf", Display: map[string]*lang.Value{"title": fromValue("workflow.outputs.title")}},
	}
	// No outputs at all: the projection resolves to nothing, so the prior
	// title survives rather than being replaced with a blank.
	s := &domain.Session{Name: "org/repo-2", Workflow: "wf"}
	cached := cachedInfo{Title: "from-cache"}
	applyDisplay(workflows, s, &cached)
	if cached.Title != "from-cache" {
		t.Errorf("Title = %q, want unchanged fallback", cached.Title)
	}
}

// Each instance of one document is evaluated against its own recorded state,
// so two instances of the same task can differ.
func TestTaskViews_EvaluatesDoneWhenPerInstanceState(t *testing.T) {
	success := "SUCCESS"
	docs := map[string]config.TaskDocument{
		"review": {
			ID: "review",
			DoneWhen: &config.DoneWhen{All: []config.DoneWhenLeaf{
				{Check: "self.state.checks_status", Eq: &success},
			}},
		},
	}
	s := &domain.Session{
		Name: "org/repo-1",
		Tasks: map[string]*contract.TaskState{
			"review#1": {
				Scope:   contract.TaskScopeSession,
				Status:  contract.TaskStatusProduced,
				TaskID:  "review",
				Dynamic: true,
				Seq:     1,
				State:   map[string]any{"checks_status": "SUCCESS"},
			},
			"review#2": {
				Scope:   contract.TaskScopeSession,
				Status:  contract.TaskStatusProduced,
				TaskID:  "review",
				Dynamic: true,
				Seq:     2,
				State:   map[string]any{"checks_status": "FAILURE"},
			},
		},
	}

	views := taskViews(&config.Config{}, taskDeclarations{docs: docs}, s, map[string]*domain.Session{s.Name: s})
	if len(views) != 2 {
		t.Fatalf("len(taskViews) = %d, want 2", len(views))
	}
	if views[0].Instance != "review#1" || views[0].DoneWhen == nil || views[0].DoneWhen.Overall != task.DoneSatisfied {
		t.Fatalf("review#1 view = %+v, want satisfied", views[0])
	}
	if views[1].Instance != "review#2" || views[1].DoneWhen == nil || views[1].DoneWhen.Overall != task.DoneUnsatisfied {
		t.Fatalf("review#2 view = %+v, want unsatisfied", views[1])
	}
	if got := views[1].DoneWhen.Leaves[0].Value; got != "FAILURE" {
		t.Errorf("review#2 checks_status value = %q, want FAILURE", got)
	}
}

func TestShowAndListExposePerRuntimeTaskDoneWhen(t *testing.T) {
	store := testStore(t)
	extra := `
[[done_when.all]]
check = "resource.state.checks_status"
eq = "SUCCESS"
`
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "review", scope: "session", extra: extra}},
		[]nodeFixture{{id: "review"}})
	seedSession(t, store, "org/repo-1", "org/repo", 1, "wf", map[string]*contract.TaskState{
		"review#1": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "review", Dynamic: true, Seq: 1, Observed: observedFacts(map[string]any{"checks_status": "SUCCESS"})},
		"review#2": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "review", Dynamic: true, Seq: 2, Observed: observedFacts(map[string]any{"checks_status": "FAILURE"})},
	})

	status, err := Status(cfg, store, "org/repo-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	assertRuntimeDoneWhenWork(t, status.Work)

	list, err := List(cfg, store)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("List returned no entries")
	}
	assertRuntimeDoneWhenViews(t, list[0].Tasks)
}

func assertRuntimeDoneWhenViews(t *testing.T, views []TaskInstanceView) {
	t.Helper()
	if len(views) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(views))
	}
	if views[0].Instance != "review#1" || views[0].DoneWhen == nil || views[0].DoneWhen.Overall != task.DoneSatisfied {
		t.Fatalf("review#1 view = %+v, want satisfied", views[0])
	}
	if views[1].Instance != "review#2" || views[1].DoneWhen == nil || views[1].DoneWhen.Overall != task.DoneUnsatisfied {
		t.Fatalf("review#2 view = %+v, want unsatisfied", views[1])
	}
}

func assertRuntimeDoneWhenWork(t *testing.T, work []StatusTask) {
	t.Helper()
	if len(work) != 2 {
		t.Fatalf("len(work) = %d, want 2", len(work))
	}
	if work[0].Instance != "review#1" || work[0].DoneWhen == nil || work[0].DoneWhen.Overall != task.DoneSatisfied {
		t.Fatalf("review#1 work = %+v, want satisfied", work[0])
	}
	if work[1].Instance != "review#2" || work[1].DoneWhen == nil || work[1].DoneWhen.Overall != task.DoneUnsatisfied {
		t.Fatalf("review#2 work = %+v, want unsatisfied", work[1])
	}
}
