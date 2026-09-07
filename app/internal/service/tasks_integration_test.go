//go:build integration

package service

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// dependsOn returns an `inputs` map that wires a node to upstream nodes, which
// is what CompileWorkflow derives its DAG edges from. The synthetic
// `_dep_<id>` key is never read by setup or cleanup — the edge follows from
// the path alone — and it carries a default so an upstream that publishes no
// such output still resolves.
func dependsOn(upstreams ...string) map[string]*lang.Value {
	out := make(map[string]*lang.Value, len(upstreams))
	for _, u := range upstreams {
		out["_dep_"+u] = fromValueOr("nodes."+u+".outputs._link", "")
	}
	return out
}

// TestIntegration_UpAutoCreatesFromURL verifies the docker compose-style ergonomic:
// `plect up <URL>` on a never-before-seen URL creates the workspace directory + state
// entry and runs both session-scoped and run-scoped setup in one shot.
func TestIntegration_UpAutoCreatesFromURL(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{"path":".env"}'`, cleanup: "true"},
			{id: "tmux", scope: "run", setup: `echo '{"session_name":"t"}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/55"
	sessionName := "testowner/testrepo-55+default"

	if store.Get(sessionName) != nil {
		t.Fatal("precondition: state should be empty")
	}

	result, err := Up(cfg, store, UpParams{Identifier: url})
	if err != nil {
		t.Fatalf("Up auto-create failed: %v", err)
	}
	if result.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", result.SessionName, sessionName)
	}

	session := store.Get(sessionName)
	if session == nil {
		t.Fatal("expected state entry created by Up")
	}
	if e := session.Tasks["envfile"]; e == nil || e.Status != contract.TaskStatusProduced {
		t.Fatalf("envfile status = %v, want produced", e)
	}
	if e := session.Tasks["tmux"]; e == nil || e.Status != contract.TaskStatusProduced {
		t.Fatalf("tmux status = %v, want produced", e)
	}
}

// TestIntegration_UpRecoversIncompleteSessionTask verifies that when state exists
// but a session-scoped task is not yet "produced" (failed/cleaned/absent),
// `plect up <URL>` invokes Create to retry the session-scoped setup before
// running run-scoped tasks. This guards the case where a previous auto-create
// partially failed and the user retries with `plect up <URL>`.
func TestIntegration_UpRecoversIncompleteSessionTask(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	markerDir := t.TempDir()
	envMarker := markerDir + "/env-ok"

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			// envfile: succeeds only when the marker exists. First create
			// attempt → marker absent → setup creates it then exits 1 →
			// status=failed. Second attempt (via Up auto-create-retry) →
			// marker exists → exit 0 → status=produced.
			{id: "envfile", scope: "session",
				setup: "test -f " + envMarker + ` && echo '{"ok":true}' || { touch ` + envMarker + "; exit 1; }"},
			{id: "tmux", scope: "run", setup: `echo '{"session_name":"t"}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux", inputs: dependsOn("envfile")}},
	)

	url := "https://github.com/testowner/testrepo/issues/66"
	sessionName := "testowner/testrepo-66+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err == nil {
		t.Fatal("expected first Create to error")
	}
	session := store.Get(sessionName)
	if session == nil {
		t.Fatal("expected state entry after partial create")
	}
	if e := session.Tasks["envfile"]; e == nil || e.Status != contract.TaskStatusFailed {
		t.Fatalf("envfile status after partial create = %v, want failed", e)
	}

	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("Up recovery failed: %v", err)
	}
	session = store.Get(sessionName)
	if e := session.Tasks["envfile"]; e == nil || e.Status != contract.TaskStatusProduced {
		t.Fatalf("envfile status after Up = %v, want produced", e)
	}
	if e := session.Tasks["tmux"]; e == nil || e.Status != contract.TaskStatusProduced {
		t.Fatalf("tmux status after Up = %v, want produced", e)
	}
}

// TestIntegration_UpWithTagDerivesTaggedSession verifies that passing the same
// URL and tag to Up resolves to the tagged session, and auto-create propagates
// the tag so the docker-compose ergonomic works for tag variants too.
func TestIntegration_UpWithTagDerivesTaggedSession(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{id: "tmux", scope: "run", setup: `echo '{}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/111"
	taggedSession := "testowner/testrepo-111+failtest"

	result, err := Up(cfg, store, UpParams{Identifier: url, Tag: "failtest"})
	if err != nil {
		t.Fatalf("Up with tag failed: %v", err)
	}
	if result.SessionName != taggedSession {
		t.Fatalf("SessionName = %q, want %q", result.SessionName, taggedSession)
	}
	if store.Get(taggedSession) == nil {
		t.Fatal("expected tagged state entry created by Up")
	}
	if store.Get("testowner/testrepo-111+default") != nil {
		t.Fatal("untagged session was created unexpectedly")
	}

	result, err = Up(cfg, store, UpParams{Identifier: url, Tag: "failtest"})
	if err != nil {
		t.Fatalf("second Up with tag failed: %v", err)
	}
	if result.SessionName != taggedSession {
		t.Fatalf("second Up SessionName = %q, want %q", result.SessionName, taggedSession)
	}
}

// TestIntegration_UpTagRejectedWithSessionName guards the strict-from-the-start
// contract: combining --tag with a bare session name returns ErrInvalidTag
// rather than silently ignoring or composing in surprising ways.
func TestIntegration_UpTagRejectedWithSessionName(t *testing.T) {
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}

	_, err := Up(cfg, store, UpParams{Identifier: "testowner/testrepo-111+default", Tag: "failtest"})
	if err == nil {
		t.Fatal("expected ErrInvalidTag when --tag is combined with a session name")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrInvalidTag {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrInvalidTag)
	}
}

// TestIntegration_UpRejectsUnknownSessionName guards the asymmetric contract: bare
// session names cannot trigger auto-create because Create needs a URL to
// resolve the branch.
func TestIntegration_UpRejectsUnknownSessionName(t *testing.T) {
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}

	_, err := Up(cfg, store, UpParams{Identifier: "testowner/testrepo-999+default"})
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

// TestIntegration_CreateIdempotent verifies that running Create twice against the
// same URL is safe: the second call reuses the existing workspace directory + state
// entry and retries any session-scoped tasks that didn't reach
// "produced" on the first attempt. Already-produced tasks must not
// re-run.
func TestIntegration_CreateIdempotent(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	stateDir := t.TempDir()
	store := state.NewStore(stateDir)

	markerDir := t.TempDir()
	markerA := markerDir + "/a-ran"
	markerB := markerDir + "/b-ran"

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "a", scope: "session", setup: "touch " + markerA + " && echo '{}'", cleanup: "true"},
			{id: "b", scope: "session",
				setup: "test -f " + markerB + ` && echo '{"ok":true}' || { touch ` + markerB + "; exit 1; }"},
		},
		[]nodeFixture{{id: "a"}, {id: "b"}},
	)

	url := "https://github.com/testowner/testrepo/issues/77"
	sessionName := "testowner/testrepo-77+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err == nil {
		t.Fatal("expected first Create to error on b's setup failure")
	}
	session := store.Get(sessionName)
	if session == nil {
		t.Fatal("state entry should exist after partial create")
	}
	if a := session.Tasks["a"]; a == nil || a.Status != contract.TaskStatusProduced {
		t.Fatalf("a status = %v, want produced", a)
	}
	if b := session.Tasks["b"]; b == nil || b.Status != contract.TaskStatusFailed {
		t.Fatalf("b status = %v, want failed", b)
	}

	result, err := Create(cfg, store, CreateParams{URL: url})
	if err != nil {
		t.Fatalf("second Create error: %v", err)
	}
	if result.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", result.SessionName, sessionName)
	}
	session = store.Get(sessionName)
	if a := session.Tasks["a"]; a == nil || a.Status != contract.TaskStatusProduced {
		t.Fatalf("a status after retry = %v, want produced", a)
	}
	if b := session.Tasks["b"]; b == nil || b.Status != contract.TaskStatusProduced {
		t.Fatalf("b status after retry = %v, want produced", b)
	}
}

// TestIntegration_DownUpPreservesPrev verifies that run-scoped task outputs survive
// `plect down → up` so setup scripts can read .Prev to keep stable identity
// (the claude task's `--resume <session_id>` mechanism, in practice).
func TestIntegration_DownUpPreservesPrev(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	// claude_like (task filenames disallow hyphens): on first setup emit a
	// fresh id, on subsequent runs emit whatever id was in .Prev. Mirrors the
	// production claude script.
	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{
				id:      "claude_like",
				scope:   "run",
				setup:   `PREV='{{get .Prev "session_id" ""}}'; SID=${PREV:-fresh-abc}; echo "{\"session_id\":\"$SID\"}"`,
				cleanup: "true",
			},
		},
		[]nodeFixture{{id: "envfile"}, {id: "claude_like"}},
	)

	url := "https://github.com/testowner/testrepo/issues/88"
	sessionName := "testowner/testrepo-88+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("first up: %v", err)
	}
	firstSID, _ := store.Get(sessionName).Tasks["claude_like"].Outputs["session_id"].(string)
	if firstSID != "fresh-abc" {
		t.Fatalf("first setup session_id = %q, want fresh-abc", firstSID)
	}

	if _, err := Down(cfg, store, DownParams{Identifier: url}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if s := store.Get(sessionName).Tasks["claude_like"].Status; s != contract.TaskStatusCleaned {
		t.Fatalf("claude_like.Status after down = %q, want cleaned", s)
	}

	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("second up: %v", err)
	}
	secondSID, _ := store.Get(sessionName).Tasks["claude_like"].Outputs["session_id"].(string)
	if secondSID != firstSID {
		t.Fatalf("session_id after down→up = %q, want %q (Prev should preserve)", secondSID, firstSID)
	}
}

// TestIntegration_DownSurvivesPartialSetup verifies that a setup that fails before
// populating outputs does not strand the session: cleanup of that task
// renders its missing-key references as empty rather than aborting, so
// `plect down` can still flip all tasks to `cleaned`.
func TestIntegration_DownSurvivesPartialSetup(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{id: "broken", scope: "run",
				setup:   `exit 1`,
				cleanup: `kill -TERM "{{.Self.pid}}" 2>/dev/null || true; rm -f "{{.Self.path}}"`},
		},
		[]nodeFixture{{id: "envfile"}, {id: "broken"}},
	)
	url := "https://github.com/testowner/testrepo/issues/99"
	sessionName := "testowner/testrepo-99+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Up(cfg, store, UpParams{Identifier: url}); err == nil {
		t.Fatal("expected Up to fail on broken task")
	}
	if s := store.Get(sessionName).Tasks["broken"].Status; s != contract.TaskStatusFailed {
		t.Fatalf("broken.Status = %q, want failed", s)
	}

	if _, err := Down(cfg, store, DownParams{Identifier: url}); err != nil {
		t.Fatalf("down should survive partial setup, got: %v", err)
	}
	if s := store.Get(sessionName).Tasks["broken"].Status; s != contract.TaskStatusCleaned {
		t.Fatalf("broken.Status after down = %q, want cleaned", s)
	}
}

func TestIntegration_CreatePropagatesInputToTemplates(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session",
				setup:   `echo "{\"template\":\"{{.SessionInputs.template}}\"}"`,
				cleanup: `echo "cleanup-saw-{{.SessionInputs.template}}" >/dev/null`},
		},
		[]nodeFixture{{id: "envfile"}},
	)

	url := "https://github.com/testowner/testrepo/issues/200"
	sessionName := "testowner/testrepo-200+default"

	if _, err := Create(cfg, store, CreateParams{URL: url, Inputs: map[string]any{"template": "review"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	session := store.Get(sessionName)
	if session == nil {
		t.Fatal("expected state entry")
	}
	if got := session.Inputs["template"]; got != "review" {
		t.Fatalf("session.Inputs[template] = %v, want review", got)
	}
	outputs := session.Tasks["envfile"].Outputs
	if outputs["template"] != "review" {
		t.Fatalf("envfile.Outputs[template] = %v, want review (proves .Input reached setup)", outputs["template"])
	}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: url, Force: true}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}

func TestIntegration_CreateRejectsInputAgainstSchema(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "envfile"}},
	)
	// Apply the global-level inputs_schema as a fallback shape (workflows
	// without their own [inputs_schema] inherit it via resolveSessionInputs).
	cfg.InputsSchema = map[string]any{
		"type":     "object",
		"required": []any{"template"},
		"properties": map[string]any{
			"template": map[string]any{"type": "string", "enum": []any{"review", "respond"}},
		},
	}

	url := "https://github.com/testowner/testrepo/issues/201"

	_, err := Create(cfg, store, CreateParams{URL: url, Inputs: map[string]any{"template": "wat"}})
	if err == nil {
		t.Fatal("expected schema validation failure")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrInvalidInput {
		t.Fatalf("err code = %v, want %q", err, ErrInvalidInput)
	}
}

func TestIntegration_UpAutoCreateWithInput(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session",
				setup: `echo "{\"template\":\"{{.SessionInputs.template}}\"}"`},
			{id: "tmux", scope: "run", setup: `echo '{}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/202"
	sessionName := "testowner/testrepo-202+default"

	if _, err := Up(cfg, store, UpParams{Identifier: url, Inputs: map[string]any{"template": "respond"}}); err != nil {
		t.Fatalf("up auto-create: %v", err)
	}
	session := store.Get(sessionName)
	if session.Inputs["template"] != "respond" {
		t.Fatalf("session.Inputs[template] = %v, want respond", session.Inputs["template"])
	}
	if session.Tasks["envfile"].Outputs["template"] != "respond" {
		t.Fatalf("envfile.Outputs[template] = %v, want respond", session.Tasks["envfile"].Outputs["template"])
	}
}

func TestIntegration_DestroyRefusesWhenWorkspaceDirDirty(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "envfile"}},
	)

	url := "https://github.com/testowner/testrepo/issues/256"
	sessionName := "testowner/testrepo-256+default"

	createResult, err := Create(cfg, store, CreateParams{URL: url})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	untracked := createResult.WorkspaceDirPath + "/dirty.txt"
	if err := os.WriteFile(untracked, []byte("uncommitted work"), 0o644); err != nil {
		t.Fatalf("seed untracked file: %v", err)
	}

	_, destroyErr := Destroy(cfg, store, DestroyParams{Identifier: sessionName})
	if destroyErr == nil {
		t.Fatal("expected Destroy without --force to fail when workspace directory has untracked files")
	}
	if msg := destroyErr.Error(); strings.Contains(msg, "use --force to delete it") {
		t.Errorf("error message duplicates git's stderr: %q", msg)
	}
	if store.Get(sessionName) == nil {
		t.Fatal("state entry should be preserved when workspace directory removal is refused")
	}
	if _, err := os.Stat(createResult.WorkspaceDirPath); err != nil {
		t.Fatalf("workspace directory should remain on disk after refusal, stat err: %v", err)
	}
	if _, err := os.Stat(untracked); err != nil {
		t.Fatalf("untracked file should remain on disk after refusal, stat err: %v", err)
	}

	// --force means "I accept losing the dirty work": it overrides the
	// provider's own refusal and forwards that intent to the release script,
	// which discards the uncommitted file along with the rest of the worktree.
	result, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName, Force: true})
	if err != nil {
		t.Fatalf("Destroy --force failed: %v", err)
	}
	if store.Get(sessionName) != nil {
		t.Fatal("state entry should be deleted after --force")
	}
	if len(result.CleanupWarnings) != 0 {
		t.Errorf("a successful forced release should not be reported as a warning, got %v", result.CleanupWarnings)
	}
	if !result.RemovedWorkspaceDir {
		t.Error("RemovedWorkspaceDir should be true after --force removes the workspace directory")
	}
	if _, err := os.Stat(createResult.WorkspaceDirPath); !os.IsNotExist(err) {
		t.Fatalf("workspace directory should be removed after --force, stat err: %v", err)
	}
	if _, err := os.Stat(untracked); !os.IsNotExist(err) {
		t.Errorf("uncommitted work should be discarded after --force, stat err: %v", err)
	}
}

// TestIntegration_DestroyAutoDownsLiveRunTask locks in the auto-down
// behavior: calling `plect destroy` on an `up` session — one with a
// run-scoped task in `produced` status — must run run-scoped cleanup
// *before* session-scoped cleanup, then remove the workspace directory and delete
// the state entry, without the user having to `plect down` first.
func TestIntegration_DestroyAutoDownsLiveRunTask(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	logFile := t.TempDir() + "/cleanup.log"

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`,
				cleanup: "echo session >> " + logFile},
			{id: "tmux", scope: "run", setup: `echo '{}'`,
				cleanup: "echo run >> " + logFile},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/300"
	sessionName := "testowner/testrepo-300+default"

	createResult, err := Create(cfg, store, CreateParams{URL: url})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("up: %v", err)
	}
	session := store.Get(sessionName)
	if e := session.Tasks["tmux"]; e == nil || e.Status != contract.TaskStatusProduced {
		t.Fatalf("precondition: tmux status = %v, want produced (need a live run task)", e)
	}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName}); err != nil {
		t.Fatalf("destroy on live session failed: %v", err)
	}

	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected cleanup log, read err: %v", err)
	}
	gotOrder := strings.Fields(string(logBytes))
	wantOrder := []string{"run", "session"}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("cleanup order = %v, want %v (run-scoped must precede session-scoped)", gotOrder, wantOrder)
	}
	if store.Get(sessionName) != nil {
		t.Fatal("state entry should be deleted after Destroy")
	}
	if _, err := os.Stat(createResult.WorkspaceDirPath); !os.IsNotExist(err) {
		t.Errorf("workspace directory should be removed, stat err: %v", err)
	}
}

// TestIntegration_DestroyForcePartialFailureContinuesTeardown locks in
// the --force behavior: a cleanup script that exits 1 is demoted to a
// CleanupWarnings entry, and the remaining teardown steps (session
// cleanup, workspace directory removal, state deletion) still complete.
func TestIntegration_DestroyForcePartialFailureContinuesTeardown(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	markerDir := t.TempDir()
	sessionCleanupMarker := markerDir + "/session-cleanup-ran"

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`,
				cleanup: "touch " + sessionCleanupMarker},
			{id: "tmux", scope: "run", setup: `echo '{}'`, cleanup: `exit 1`},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/301"
	sessionName := "testowner/testrepo-301+default"

	createResult, err := Create(cfg, store, CreateParams{URL: url})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("up: %v", err)
	}

	result, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName, Force: true})
	if err != nil {
		t.Fatalf("Destroy --force returned error, want nil: %v", err)
	}
	if len(result.CleanupWarnings) == 0 {
		t.Fatal("expected CleanupWarnings to record run cleanup failure")
	}
	// Single reverse-instantiation teardown: warnings are prefixed
	// "cleanup:" and name the failing task (tmux) rather than a per-scope label.
	if joined := strings.Join(result.CleanupWarnings, "|"); !strings.Contains(joined, "tmux") {
		t.Fatalf("expected tmux cleanup failure in warnings, got %q", joined)
	}
	if _, err := os.Stat(sessionCleanupMarker); err != nil {
		t.Errorf("session-scoped cleanup did not run after run cleanup failure: %v", err)
	}
	if !result.RemovedWorkspaceDir {
		t.Errorf("RemovedWorkspaceDir = false, want true (workspace directory removal must continue under --force)")
	}
	if _, err := os.Stat(createResult.WorkspaceDirPath); !os.IsNotExist(err) {
		t.Errorf("workspace directory should be removed under --force, stat err: %v", err)
	}
	if store.Get(sessionName) != nil {
		t.Fatal("state entry should be deleted under --force despite cleanup failure")
	}
}

// TestIntegration_AttachResolvesRenderedCommand verifies the full happy path
// for `plect attach`: a create → up sequence produces the attach task's
// outputs, and Attach renders the declared template against those outputs to
// produce the exact command the CLI will exec.
func TestIntegration_AttachResolvesRenderedCommand(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{
				id:      "tmux",
				scope:   "run",
				setup:   `echo '{"session_name":"owner-7"}'`,
				cleanup: "true",
				attach:  "tmux attach -t {{.Self.session_name}}",
			},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/7"
	sessionName := "testowner/testrepo-7+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("up: %v", err)
	}

	result, err := Attach(cfg, store, AttachParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if result.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", result.SessionName, sessionName)
	}
	if result.TaskID != "tmux" {
		t.Fatalf("TaskID = %q, want %q", result.TaskID, "tmux")
	}
	// The command is the resolved verb, not its source: a shell verb is
	// materialized, so what the caller hands to its shell is a path. Running
	// it is what says the verb was resolved against this instance.
	out, err := exec.Command("bash", "-c", result.Command).CombinedOutput()
	if err != nil {
		t.Fatalf("run the resolved attach command: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "attach -t owner-7" {
		t.Fatalf("resolved attach ran %q, want the declaring instance's own pane", got)
	}
}

// TestIntegration_AttachAbortsWhenTaskNotProduced verifies the "no auto-up"
// stance: Attach against a never-produced (or downed) task returns
// ErrNotProduced with a hint pointing at `plect up`, instead of silently
// invoking setup.
func TestIntegration_AttachAbortsWhenTaskNotProduced(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{id: "tmux", scope: "run",
				setup:   `echo '{"session_name":"s"}'`,
				cleanup: "true",
				attach:  "tmux attach -t {{.Self.session_name}}"},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/8"
	sessionName := "testowner/testrepo-8+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := Attach(cfg, store, AttachParams{Identifier: sessionName})
	if err == nil {
		t.Fatal("expected ErrNotProduced when attach target hasn't run")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrNotProduced {
		t.Fatalf("err = %v (%T), want code=%q", err, err, ErrNotProduced)
	}
	if !strings.Contains(svcErr.Message, "plect up") {
		t.Fatalf("error message %q should hint at 'plect up'", svcErr.Message)
	}
}

// TestIntegration_AttachWithoutDeclarationFails verifies the abort path for a
// workflow with no attach target — the user gets a clear error rather than a
// silent no-op or a confusing template error.
func TestIntegration_AttachWithoutDeclarationFails(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "envfile"}},
	)

	url := "https://github.com/testowner/testrepo/issues/9"
	sessionName := "testowner/testrepo-9+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := Attach(cfg, store, AttachParams{Identifier: sessionName})
	if err == nil {
		t.Fatal("expected ErrNotAttachable when no task declares attach")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrNotAttachable {
		t.Fatalf("err = %v (%T), want code=%q", err, err, ErrNotAttachable)
	}
}

// TestIntegration_CaptureResolvesRenderedOutput verifies the full happy path
// for `plect capture`: a create → up sequence produces the capture task's
// outputs, and Capture renders the declared template against those outputs
// and returns its stdout unmodified.
func TestIntegration_CaptureResolvesRenderedOutput(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{
				id:      "tmux",
				scope:   "run",
				setup:   `echo '{"session_name":"owner-701"}'`,
				cleanup: "true",
				attach:  "tmux attach -t {{.Self.session_name}}",
				capture: `printf '%s' "view of {{.Self.session_name}}"`,
			},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/701"
	sessionName := "testowner/testrepo-701+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("up: %v", err)
	}

	result, err := Capture(cfg, store, CaptureParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if result.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", result.SessionName, sessionName)
	}
	if result.TaskID != "tmux" {
		t.Fatalf("TaskID = %q, want %q", result.TaskID, "tmux")
	}
	if result.Content != "view of owner-701" {
		t.Fatalf("Content = %q, want %q", result.Content, "view of owner-701")
	}
}

// TestIntegration_CaptureAbortsWhenTaskNotProduced mirrors the "no auto-up"
// stance from Attach: capture against a never-produced (or downed) task
// returns ErrNotProduced with a hint pointing at `plect up`, instead of an
// empty snapshot.
func TestIntegration_CaptureAbortsWhenTaskNotProduced(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{id: "tmux", scope: "run",
				setup:   `echo '{"session_name":"s"}'`,
				cleanup: "true",
				capture: "echo hi"},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/702"
	sessionName := "testowner/testrepo-702+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := Capture(cfg, store, CaptureParams{Identifier: sessionName})
	if err == nil {
		t.Fatal("expected ErrNotProduced when capture target hasn't run")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrNotProduced {
		t.Fatalf("err = %v (%T), want code=%q", err, err, ErrNotProduced)
	}
	if !strings.Contains(svcErr.Message, "plect up") {
		t.Fatalf("error message %q should hint at 'plect up'", svcErr.Message)
	}
}

// TestIntegration_CaptureWithoutDeclarationFails verifies the abort path for
// a workflow with no capture target — the user gets a clear error rather
// than an empty-output success.
func TestIntegration_CaptureWithoutDeclarationFails(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "envfile"}},
	)

	url := "https://github.com/testowner/testrepo/issues/703"
	sessionName := "testowner/testrepo-703+default"

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := Capture(cfg, store, CaptureParams{Identifier: sessionName})
	if err == nil {
		t.Fatal("expected ErrNotCapturable when no task declares capture")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrNotCapturable {
		t.Fatalf("err = %v (%T), want code=%q", err, err, ErrNotCapturable)
	}
}

// TestIntegration_MultipleTerminalDeclarationsRejectedAtPlanCompile verifies
// that a workflow where more than one task declares [terminal] fails to
// compile, instead of silently picking one — a capture-only ambiguity like
// the pre-[terminal] split's ErrAmbiguousCapture can no longer arise, since
// capture is now always declared alongside attach/send_text/send_keys on the
// same task, and a second [terminal]-declaring node is already rejected here.
// The plan compiles as part of Create itself (session-scoped setup needs
// the full plan), so the rejection surfaces there rather than at Up.
func TestIntegration_MultipleTerminalDeclarationsRejectedAtPlanCompile(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{id: "tmux", scope: "run", setup: `echo '{"session_name":"s"}'`, cleanup: "true", capture: "echo a"},
			{id: "other", scope: "run", setup: `echo '{}'`, cleanup: "true", capture: "echo b"},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}, {id: "other"}},
	)

	url := "https://github.com/testowner/testrepo/issues/704"

	_, err := Create(cfg, store, CreateParams{URL: url})
	if err == nil {
		t.Fatal("expected plan compile to reject a second [terminal]-declaring node")
	}
	if !strings.Contains(err.Error(), "more than one node declares [terminal]") {
		t.Fatalf("err = %v, want a more-than-one-[terminal] message", err)
	}
}

// TestIntegration_CaptureSurfacesScriptFailure verifies AC3's "pane gone"
// case: the declared script failing (e.g. an orphaned tmux pane) must be a
// hard error carrying stderr, not an empty-output success.
func TestIntegration_CaptureSurfacesScriptFailure(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workspaceDirsRoot, "default",
		[]taskFixture{
			{id: "envfile", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{id: "tmux", scope: "run",
				setup:   `echo '{"session_name":"s"}'`,
				cleanup: "true",
				capture: `echo "can't find pane" >&2; exit 1`},
		},
		[]nodeFixture{{id: "envfile"}, {id: "tmux"}},
	)

	url := "https://github.com/testowner/testrepo/issues/705"
	sessionName := "testowner/testrepo-705+default"

	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("up: %v", err)
	}
	_, err := Capture(cfg, store, CaptureParams{Identifier: sessionName})
	if err == nil {
		t.Fatal("expected ErrExecutionFailed when the capture script fails")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrExecutionFailed {
		t.Fatalf("err = %v (%T), want code=%q", err, err, ErrExecutionFailed)
	}
	if !strings.Contains(svcErr.Message, "can't find pane") {
		t.Fatalf("error message %q should carry the script's stderr", svcErr.Message)
	}
}

// TestIntegration_WorkflowFile_NodeWiring exercises the workflow file path end
// to end: a workflow at .plect/workflows/<name>.toml referencing task
// definitions at .plect/tasks/<id>.toml, with the DAG derived from
// `.Nodes.<id>.outputs.<key>` references in the node input bindings.
//
// Asserts that:
//   - Session.Workflow is frozen on the new session
//   - upstream node outputs surface in downstream `.Input.<key>`
//   - TaskState.Inputs is persisted (so cleanup can run without CLI re-entry)
func TestIntegration_WorkflowFile_NodeWiring(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	ownerRepo := "testowner/testrepo"
	repoDir := t.TempDir() + "/config"
	if err := os.MkdirAll(repoDir+"/.plect/tasks", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/tasks/tmux.toml", []byte(`
[tmux]
kind  = "effect"
scope = "run"

[tmux.setup]
type   = "shell"
script = "echo '{\"session_name\":\"abc\"}'"

[tmux.health.alive]
type = "noop"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/tasks/agent.toml", []byte(`
[agent]
kind  = "effect"
scope = "run"

[agent.setup]
type   = "shell"
script = "jq -nc --arg target \"$session_name\" '{target:$target}'"

[agent.setup.bind]
session_name = { from = "inputs.session_name" }

[agent.health.alive]
type = "noop"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoDir+"/.plect/workflows", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/workflows/coding.toml", []byte(`
[coding]
kind = "workflow"
name = "coding"

[[coding.nodes]]
uses = "tmux"

[[coding.nodes]]
uses = "agent"

[coding.nodes.inputs]
session_name = { from = "nodes.tmux.outputs.session_name" }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{WorkspaceDirsRoot: workspaceDirsRoot, BaseDir: repoDir + "/.plect"}
	attachGithubWorkspaceProvider(t, cfg, "coding")
	url := "https://github.com/" + ownerRepo + "/issues/99"
	sessionName := "testowner/testrepo-99+coding"

	result, err := Up(cfg, store, UpParams{Identifier: url, Workflow: "coding"})
	if err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	if result.SessionName != sessionName {
		t.Fatalf("SessionName = %q, want %q", result.SessionName, sessionName)
	}

	session := store.Get(sessionName)
	if session == nil {
		t.Fatal("expected state entry")
	}
	if session.Workflow != "coding" {
		t.Errorf("Session.Workflow = %q, want coding", session.Workflow)
	}
	tmux := session.Tasks["tmux"]
	if tmux == nil || tmux.Status != contract.TaskStatusProduced {
		t.Fatalf("tmux state = %+v, want produced", tmux)
	}
	if tmux.Outputs["session_name"] != "abc" {
		t.Fatalf("tmux outputs = %+v, want session_name=abc", tmux.Outputs)
	}
	agent := session.Tasks["agent"]
	if agent == nil || agent.Status != contract.TaskStatusProduced {
		t.Fatalf("agent state = %+v, want produced", agent)
	}
	if got := agent.Inputs["session_name"]; got != "abc" {
		t.Errorf("agent.Inputs.session_name = %v, want abc (rendered from upstream output)", got)
	}
	if agent.Outputs["target"] != "abc" {
		t.Errorf("agent.Outputs.target = %v, want abc (came in via .Inputs.session_name)", agent.Outputs)
	}
}

// TestIntegration_WorkflowFrozenOnSession verifies that once a workflow is
// chosen at create time, `plect up --workflow X` against the existing session
// returns ErrInvalidInput when X differs — the plan must not silently switch
// out from under a session.
func TestIntegration_WorkflowFrozenOnSession(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	ownerRepo := "testowner/testrepo"
	repoDir := t.TempDir() + "/config"
	if err := os.MkdirAll(repoDir+"/.plect/workflows", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoDir+"/.plect/tasks", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/tasks/noop.toml", []byte(`
[noop]
kind  = "effect"
scope = "session"

[noop.setup]
type   = "shell"
script = "echo '{}'"

[noop.health.alive]
type = "noop"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/workflows/a.toml", []byte(`
[a]
kind = "workflow"
name = "a"

[[a.nodes]]
uses = "noop"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/workflows/b.toml", []byte(`
[b]
kind = "workflow"
name = "b"

[[b.nodes]]
uses = "noop"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{WorkspaceDirsRoot: workspaceDirsRoot, BaseDir: repoDir + "/.plect"}
	attachGithubWorkspaceProvider(t, cfg, "a")
	attachGithubWorkspaceProvider(t, cfg, "b")
	url := "https://github.com/" + ownerRepo + "/issues/77"

	if _, err := Create(cfg, store, CreateParams{URL: url, Workflow: "a"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The frozen workflow is checked against the session, so the mismatch has
	// to be raised on the session itself: a different workflow on the resource
	// resolves to a different (tagged) session name entirely.
	_, err := Up(cfg, store, UpParams{Identifier: "testowner/testrepo-77+a", Workflow: "b"})
	if err == nil {
		t.Fatal("expected ErrInvalidInput when --workflow mismatches frozen workflow")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrInvalidInput {
		t.Fatalf("err = %v (%T), want ErrInvalidInput", err, err)
	}
}

// TestIntegration_AttachUnderWorkflowPath guards against the regression where
// Attach reached for Resolved.Config (empty under the workflow path) and
// looked up session.Tasks[""]. Exercises a workflow file whose tmux node
// declares `attach`, runs `plect up`, then verifies Attach resolves the right
// task id and renders the command using the node's own outputs.
func TestIntegration_AttachUnderWorkflowPath(t *testing.T) {
	workspaceDirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	ownerRepo := "testowner/testrepo"
	repoDir := t.TempDir() + "/config"
	if err := os.MkdirAll(repoDir+"/.plect/tasks", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoDir+"/.plect/workflows", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/tasks/tmux.toml", []byte(`
[tmux]
kind  = "effect"
scope = "run"

[tmux.setup]
type   = "shell"
script = "echo '{\"session_name\":\"abc\"}'"

[tmux.health.alive]
type = "noop"

[tmux.terminal.attach]
type    = "exec"
command = "tmux"
args    = ["attach", "-t", { from = "self.outputs.session_name" }]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoDir+"/.plect/workflows/coding.toml", []byte(`
[coding]
kind = "workflow"
name = "coding"

[[coding.nodes]]
uses = "tmux"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{WorkspaceDirsRoot: workspaceDirsRoot, BaseDir: repoDir + "/.plect"}
	attachGithubWorkspaceProvider(t, cfg, "coding")
	url := "https://github.com/" + ownerRepo + "/issues/300"
	sessionName := "testowner/testrepo-300+coding"

	if _, err := Up(cfg, store, UpParams{Identifier: url, Workflow: "coding"}); err != nil {
		t.Fatalf("Up: %v", err)
	}

	res, err := Attach(cfg, store, AttachParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Attach failed under workflow path: %v", err)
	}
	if res.TaskID != "tmux" {
		t.Errorf("TaskID = %q, want %q", res.TaskID, "tmux")
	}
	out, err := exec.Command("bash", "-c", res.Command).CombinedOutput()
	if err != nil {
		t.Fatalf("run the resolved attach command: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "attach -t abc" {
		t.Errorf("resolved attach ran %q, want the declaring instance's own pane", got)
	}
}
