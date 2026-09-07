package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/app/internal/state"
	taskpkg "github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func TestPutBestEffort_PutFailureLogsWarningWithoutPanicking(t *testing.T) {
	dir := t.TempDir()
	// storage.db exists as a directory, so Store.Put's open of it fails —
	// this pins the best-effort swallow at putBestEffort: a broken store must
	// not panic or block the caller, but the failure must not be invisible
	// either.
	if err := os.MkdirAll(persistence.PathIn(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(dir)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	putBestEffort(store, &domain.Session{Name: "owner/repo-1"}, "test context")

	if !bytes.Contains(logs.Bytes(), []byte("best-effort session state persist failed")) {
		t.Errorf("expected a warning about the failed persist, got log output: %q", logs.String())
	}
}

func seedSession(t *testing.T, store interface {
	Put(*domain.Session) error
	Dir() string
}, sessionName, ownerRepo string, number int, workflow string, tasks map[string]*contract.TaskState) {
	t.Helper()
	now := time.Now()
	if tasks == nil {
		tasks = map[string]*contract.TaskState{}
	}
	// Never synthesize @workflow itself: Destroy treats its presence as
	// "workflow setup ran, run its cleanup too."
	if wf := tasks[contract.WorkflowPseudoNodeID]; wf != nil {
		if wf.Outputs == nil {
			wf.Outputs = map[string]any{}
		}
		if _, ok := wf.Outputs["branch"]; !ok {
			wf.Outputs["branch"] = "issue/1"
		}
	}

	session := &domain.Session{
		Name:       sessionName,
		ResourceID: fmt.Sprintf("https://github.com/%s/issues/%d", ownerRepo, number),
		Workflow:   workflow,
		Tasks:      tasks,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := store.Put(session); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func setParent(t *testing.T, store *state.Store, sessionName, parentName string) {
	t.Helper()
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.ParentSession = parentName
		return nil
	}); err != nil {
		t.Fatalf("set parent: %v", err)
	}
}

func TestResolveParentSession_RootPrefixAcceptsExistingTarget(t *testing.T) {
	store := testStore(t)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", nil)

	got, err := resolveParentSession(store, "owner/repo-reviewer", "root:owner/repo-1")
	if err != nil {
		t.Fatalf("resolveParentSession: %v", err)
	}
	if got != "root:owner/repo-1" {
		t.Fatalf("got %q, want %q (stored literally, not resolved)", got, "root:owner/repo-1")
	}
}

func TestResolveParentSession_RootPrefixRejectsMissingTarget(t *testing.T) {
	store := testStore(t)

	_, err := resolveParentSession(store, "owner/repo-reviewer", "root:owner/repo-missing")
	if err == nil {
		t.Fatal("expected error when root: target session does not exist")
	}
	if err.Code != ErrInvalidInput {
		t.Errorf("Code = %q, want %q", err.Code, ErrInvalidInput)
	}
}

func TestUp_RejectsInputWithBareSessionName(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}

	_, err := Up(cfg, store, UpParams{Identifier: "org/repo-1", Inputs: map[string]any{"template": "review"}})
	if err == nil {
		t.Fatal("expected error when --input is combined with a session name")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrInvalidInput {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrInvalidInput)
	}
}

func TestUp_RejectsInputWhenSessionExists(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	sessionName := "org/repo-9"
	seedSession(t, store, sessionName, "org/repo", 9, "", nil)

	_, err := Up(cfg, store, UpParams{Identifier: sessionName, Inputs: map[string]any{"template": "review"}})
	if err == nil {
		t.Fatal("expected error when --input is passed against an existing session")
	}
	svcErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if svcErr.Code != ErrInvalidInput {
		t.Errorf("Code = %q, want %q", svcErr.Code, ErrInvalidInput)
	}
}

func TestResolveSessionInput_NoSchemaAcceptsAnyObject(t *testing.T) {
	cfg := &config.Config{}
	got, err := resolveSessionInputs(cfg, "", "", map[string]any{"anything": "goes"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["anything"] != "goes" {
		t.Errorf("got %v, want value preserved", got)
	}
}

func TestResolveSessionInput_NilWithSchemaValidatesEmptyObject(t *testing.T) {
	cfg := &config.Config{
		InputsSchema: map[string]any{
			"type":     "object",
			"required": []any{"template"},
			"properties": map[string]any{
				"template": map[string]any{"type": "string"},
			},
		},
	}
	_, err := resolveSessionInputs(cfg, "", "", nil)
	if err == nil {
		t.Fatal("expected schema rejection for empty input")
	}
	if err.Code != ErrInvalidInput {
		t.Errorf("Code = %q, want %q", err.Code, ErrInvalidInput)
	}
}

func TestResolveSessionInput_SchemaValidates(t *testing.T) {
	cfg := &config.Config{
		InputsSchema: map[string]any{
			"type":     "object",
			"required": []any{"template"},
			"properties": map[string]any{
				"template": map[string]any{"type": "string", "enum": []any{"review", "respond"}},
			},
		},
	}

	if _, err := resolveSessionInputs(cfg, "", "", map[string]any{"template": "review"}); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	got, err := resolveSessionInputs(cfg, "", "", map[string]any{"template": "not-in-enum"})
	if err == nil {
		t.Fatalf("expected schema error, got value %v", got)
	}
	if err.Code != ErrInvalidInput {
		t.Errorf("Code = %q, want %q", err.Code, ErrInvalidInput)
	}
}

func TestResolveSessionInput_InlineAndFileMutuallyExclusive(t *testing.T) {
	cfg := &config.Config{
		InputsSchema:     map[string]any{"type": "object"},
		InputsSchemaFile: "/tmp/x.json",
	}
	_, err := resolveSessionInputs(cfg, "", "", map[string]any{})
	if err == nil {
		t.Fatal("expected error for both inline and file declared")
	}
	if !strings.Contains(err.Message, "mutually exclusive") {
		t.Errorf("Message = %q, want mutually-exclusive wording", err.Message)
	}
}

// TestDestroy_DefaultFailsFastOnRunCleanupError exercises the default policy:
// without --force, a cleanup error must abort teardown and keep the state
// entry intact so the user can inspect / retry.
// A guarded orchestrator must not be able to tear down a session outside its
// name space — the symmetric vector to the Create guard. The guard fires
// before any cleanup or workdir removal, so the target session survives.
func TestDestroy_SessionGuardBlocksCrossOwner(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{SessionGuard: "^acme/"}
	sessionName := "exampleorg/repo-26"
	seedSession(t, store, sessionName, "exampleorg/repo", 26, "default", nil)

	_, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRepoNotAllowed {
		t.Fatalf("want ErrRepoNotAllowed for cross-owner destroy, got %v", err)
	}
	if store.Get(sessionName) == nil {
		t.Fatal("rejected destroy must leave the target session intact")
	}
}

// Down is a per-session write too — the guard clamps it the same way.
func TestDown_SessionGuardBlocksCrossOwner(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{SessionGuard: "^acme/"}
	sessionName := "exampleorg/repo-26"
	seedSession(t, store, sessionName, "exampleorg/repo", 26, "default", nil)

	_, err := Down(cfg, store, DownParams{Identifier: sessionName})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRepoNotAllowed {
		t.Fatalf("want ErrRepoNotAllowed for cross-owner down, got %v", err)
	}
}

// checkLifecycleRelationGuard is exercised directly (no cfg/workflow needed)
// against every tree-relation case, including the implicit-root sibling
// wrinkle: a session opted into `--parent root:X` is X's sibling, not X's
// descendant, so X must not be able to destroy it.
func TestCheckLifecycleRelationGuard(t *testing.T) {
	newTreeStore := func(t *testing.T) *state.Store {
		store := testStore(t)
		seedSession(t, store, "org/repo-parent", "org/repo", 1, "default", nil)
		seedSession(t, store, "org/repo-child", "org/repo", 2, "default", nil)
		seedSession(t, store, "org/repo-grandchild", "org/repo", 3, "default", nil)
		seedSession(t, store, "org/repo-unrelated", "org/repo", 4, "default", nil)
		setParent(t, store, "org/repo-child", "org/repo-parent")
		setParent(t, store, "org/repo-grandchild", "org/repo-child")
		return store
	}

	t.Run("self is allowed", func(t *testing.T) {
		store := newTreeStore(t)
		t.Setenv("PLECT_SESSION_NAME", "org/repo-parent")
		if err := checkLifecycleRelationGuard(store, "org/repo-parent", "destroy"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("direct parent destroying direct child is allowed", func(t *testing.T) {
		store := newTreeStore(t)
		t.Setenv("PLECT_SESSION_NAME", "org/repo-parent")
		if err := checkLifecycleRelationGuard(store, "org/repo-child", "destroy"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("ancestor destroying a multi-level descendant is allowed", func(t *testing.T) {
		store := newTreeStore(t)
		t.Setenv("PLECT_SESSION_NAME", "org/repo-parent")
		if err := checkLifecycleRelationGuard(store, "org/repo-grandchild", "destroy"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("child destroying its parent is rejected", func(t *testing.T) {
		store := newTreeStore(t)
		t.Setenv("PLECT_SESSION_NAME", "org/repo-child")
		svcErr := checkLifecycleRelationGuard(store, "org/repo-parent", "destroy")
		if svcErr == nil || svcErr.Code != ErrRelationNotAllowed {
			t.Fatalf("want ErrRelationNotAllowed, got %v", svcErr)
		}
	})

	t.Run("unrelated session is rejected", func(t *testing.T) {
		store := newTreeStore(t)
		t.Setenv("PLECT_SESSION_NAME", "org/repo-unrelated")
		svcErr := checkLifecycleRelationGuard(store, "org/repo-parent", "destroy")
		if svcErr == nil || svcErr.Code != ErrRelationNotAllowed {
			t.Fatalf("want ErrRelationNotAllowed, got %v", svcErr)
		}
	})

	t.Run("implicit-root sibling reviewer cannot destroy the parentless session", func(t *testing.T) {
		store := testStore(t)
		seedSession(t, store, "org/repo-owner", "org/repo", 1, "default", nil) // parentless
		seedSession(t, store, "org/repo-reviewer", "org/repo", 2, "default", nil)
		setParent(t, store, "org/repo-reviewer", "root:org/repo-owner")
		t.Setenv("PLECT_SESSION_NAME", "org/repo-reviewer")
		svcErr := checkLifecycleRelationGuard(store, "org/repo-owner", "destroy")
		if svcErr == nil || svcErr.Code != ErrRelationNotAllowed {
			t.Fatalf("want ErrRelationNotAllowed (sibling via implicit root), got %v", svcErr)
		}
	})

	t.Run("no ambient session is exempt (human CLI recovery path)", func(t *testing.T) {
		store := newTreeStore(t)
		t.Setenv("PLECT_SESSION_NAME", "")
		if err := checkLifecycleRelationGuard(store, "org/repo-unrelated", "destroy"); err != nil {
			t.Fatalf("want nil (no ambient session is exempt), got %v", err)
		}
	})
}

// TestDestroy_RelationGuardBlocksUnrelatedCaller and TestDown_RelationGuardBlocksUnrelatedCaller
// exercise the guard through the service entry points: an orchestrator must
// not destroy/down a session outside its own subtree, even one it can see
// via `plect ls`.
func TestDestroy_RelationGuardBlocksUnrelatedCaller(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	sessionName := "org/repo-1"
	seedSession(t, store, sessionName, "org/repo", 1, "default", nil)
	seedSession(t, store, "org/repo-caller", "org/repo", 2, "default", nil)
	t.Setenv("PLECT_SESSION_NAME", "org/repo-caller")

	_, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRelationNotAllowed {
		t.Fatalf("want ErrRelationNotAllowed for unrelated destroy, got %v", err)
	}
	if store.Get(sessionName) == nil {
		t.Fatal("rejected destroy must leave the target session intact")
	}
}

func TestDown_RelationGuardBlocksUnrelatedCaller(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	sessionName := "org/repo-1"
	seedSession(t, store, sessionName, "org/repo", 1, "default", nil)
	seedSession(t, store, "org/repo-caller", "org/repo", 2, "default", nil)
	t.Setenv("PLECT_SESSION_NAME", "org/repo-caller")

	_, err := Down(cfg, store, DownParams{Identifier: sessionName})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRelationNotAllowed {
		t.Fatalf("want ErrRelationNotAllowed for unrelated down, got %v", err)
	}
}

// TestDestroy_RelationGuardAllowsSelf confirms the guard doesn't just reject
// correctly but also lets a legitimate self-teardown proceed to completion.
func TestDestroy_RelationGuardAllowsSelf(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "envfile", scope: "session", setup: "true", cleanup: "true"}},
		[]nodeFixture{{id: "envfile"}},
	)
	sessionName := "org/repo-1"
	seedSession(t, store, sessionName, "org/repo", 1, "default", map[string]*contract.TaskState{
		"envfile": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})
	t.Setenv("PLECT_SESSION_NAME", sessionName)

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Destroy (self): %v", err)
	}
	if store.Get(sessionName) != nil {
		t.Fatal("expected state entry deleted after self-destroy")
	}
}

// `plect up <bare-existing-session>` skips the guarded auto-create path, so the
// guard at the existing-session resolution must catch it too.
func TestUp_SessionGuardBlocksCrossOwner(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{SessionGuard: "^acme/"}
	sessionName := "exampleorg/repo-26"
	seedSession(t, store, sessionName, "exampleorg/repo", 26, "default", nil)

	_, err := Up(cfg, store, UpParams{Identifier: sessionName})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRepoNotAllowed {
		t.Fatalf("want ErrRepoNotAllowed for cross-owner up, got %v", err)
	}
}

func TestUp_ForceRecreateRelationGuardBlocksUnrelatedCaller(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	sessionName := "org/repo-1"
	seedSession(t, store, sessionName, "org/repo", 1, "default", nil)
	seedSession(t, store, "org/repo-caller", "org/repo", 2, "default", nil)
	t.Setenv("PLECT_SESSION_NAME", "org/repo-caller")

	_, err := Up(cfg, store, UpParams{Identifier: sessionName, ForceRecreate: true})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrRelationNotAllowed {
		t.Fatalf("want ErrRelationNotAllowed for unrelated force recreate, got %v", err)
	}
}

// A run-scope node's setup script (e.g. goal_bootstrap re-deriving
// `pursue_goal` instances during `plect up`, see
// config/plect/tasks/goal_bootstrap.toml) can itself shell out to a nested
// `plect task setup`, which writes its instance straight to the store while
// this Up call's own RunSetup is still in flight. Up's persist must overlay
// (mergeTasks), not blind-Put, or that nested write is clobbered — the same
// hazard TestCreate_SessionNodeNestedWriteSurvives covers for Create's
// initial_task dispatcher. Exercises the real store path (not a stub).
func TestUp_RunScopeNestedWriteSurvives(t *testing.T) {
	store := testStore(t)
	sessionName := "org/repo-12"
	t.Setenv("PLECT_SERVICE_NESTED_WRITE_HELPER", "1")

	// dispatcher mimics a nested `plect task setup`: writes a sibling "goal_x"
	// key straight to the store, then produces normally.
	dispatcher := nestedWriteCommand(t, store.Dir(), sessionName, nestedWritePatch{
		Tasks: map[string]*contract.TaskState{
			"goal_x": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Dynamic: true, TaskID: "pursue_goal", Name: "goal_x", Outputs: map[string]any{}},
		},
	}) + `
echo '{}'`
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "dispatcher", scope: "run", setup: dispatcher}},
		[]nodeFixture{{id: "dispatcher"}},
	)
	seedSession(t, store, sessionName, "org/repo", 12, "default", nil)

	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	// The dispatcher node itself produced…
	if st := result.Tasks["dispatcher"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("dispatcher node = %+v, want produced", st)
	}
	// …and its nested write survived Up's persist, both in the result and on disk.
	got := result.Tasks["goal_x"]
	if got == nil {
		t.Fatal("nested-written 'goal_x' instance was clobbered by the up persist (result)")
	}
	if got.Name != "goal_x" || got.TaskID != "pursue_goal" {
		t.Fatalf("goal_x instance mismatch: %+v", got)
	}
	persisted := store.Get(sessionName)
	if persisted == nil || persisted.Tasks["goal_x"] == nil {
		t.Fatal("nested-written 'goal_x' instance was clobbered by the up persist (disk)")
	}
}

func TestUp_ProducesSessionScopedNodeAddedAfterSessionCreation(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "org/repo-12"
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{id: "gh_guard", scope: "session", setup: `echo '{"dir":"the-guard-dir"}'`},
			{id: "tmux", scope: "run", setup: `echo '{}'`},
			{id: "claude", scope: "run", setup: `printf '{"saw":"%s"}' '{{.Nodes.gh_guard.outputs.dir}}'`},
		},
		[]nodeFixture{
			{id: "gh_guard"},
			{id: "tmux"},
			{id: "claude", inputs: map[string]*lang.Value{"path_prepend": fromValue("nodes.gh_guard.outputs.dir")}},
		},
	)
	seedSession(t, store, sessionName, "org/repo", 12, "default", map[string]*contract.TaskState{
		"tmux": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if st := result.Tasks["gh_guard"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("gh_guard = %+v, want produced", st)
	}
	if st := result.Tasks["claude"]; st == nil || st.Outputs["saw"] != "the-guard-dir" {
		t.Fatalf("claude did not resolve gh_guard's newly-produced output: %+v", st)
	}
}

func TestUp_CleansAndDropsProducedNodeRemovedFromWorkflow(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "org/repo-12"
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{
				id:      "retired",
				scope:   "run",
				cleanup: fmt.Sprintf(`printf 'retired=%%s\n' '{{.Self.token}}' >> %s`, cleanupLog),
				alive:   "false",
			},
			{
				id:      "kept",
				scope:   "run",
				setup:   fmt.Sprintf(`printf 'kept setup\n' >> %s; echo '{}'`, cleanupLog),
				cleanup: "true",
				alive:   "true",
			},
		},
		[]nodeFixture{{id: "kept"}},
	)
	seedSession(t, store, sessionName, "org/repo", 12, "default", map[string]*contract.TaskState{
		"retired": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"token": "old-token"},
			Seq:     1,
		},
		"kept": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{},
			Seq:     2,
		},
	})

	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	if st := result.Tasks["retired"]; st != nil {
		t.Fatalf("retired task = %+v, want dropped after cleanup", st)
	}
	if st := result.Tasks["kept"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("kept task = %+v, want untouched produced state", st)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "retired=old-token\n") {
		t.Fatalf("cleanup log = %q, want retired cleanup with recorded output", log)
	}
	if strings.Contains(log, "kept setup") {
		t.Fatalf("cleanup log = %q, kept node setup should have been skipped as already produced", log)
	}
	report, err := EvaluateHealth(cfg, store, sessionName)
	if err != nil {
		t.Fatalf("EvaluateHealth: %v", err)
	}
	if report.State() != domain.HealthHealthy {
		t.Fatalf("health report = %+v, want healthy after stale failing probe was dropped", report)
	}
}

func TestUp_StaleNodeCleanupDoesNotClobberConcurrentSessionWrites(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "org/repo-14"
	t.Setenv("PLECT_SERVICE_NESTED_WRITE_HELPER", "1")
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	concurrentWrite := nestedWriteCommand(t, store.Dir(), sessionName, nestedWritePatch{
		Tasks: map[string]*contract.TaskState{
			"kept": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{"self_healed": "yes"}, Seq: 2},
		},
		Health:      &contract.HealthState{LastState: "healthy", LastReason: "concurrent"},
		TickBackoff: &contract.TickBackoff{LastFingerprint: "concurrent"},
	})
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{
				id:      "retired",
				scope:   "run",
				cleanup: concurrentWrite + "\n" + fmt.Sprintf(`printf 'retired=%%s\n' '{{.Self.token}}' >> %s`, cleanupLog),
			},
			{
				id:    "kept",
				scope: "run",
				setup: fmt.Sprintf(`printf 'kept setup\n' >> %s; echo '{}'`, cleanupLog),
			},
		},
		[]nodeFixture{{id: "kept"}},
	)
	seedSession(t, store, sessionName, "org/repo", 14, "default", map[string]*contract.TaskState{
		"retired": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"token": "old-token"},
			Seq:     1,
		},
		"kept": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{},
			Seq:     2,
		},
	})

	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if st := result.Tasks["retired"]; st != nil {
		t.Fatalf("retired task = %+v, want dropped after cleanup", st)
	}
	if st := result.Tasks["kept"]; st == nil || st.Outputs["self_healed"] != "yes" {
		t.Fatalf("kept task = %+v, want concurrent output update preserved", st)
	}
	persisted := store.Get(sessionName)
	if persisted.Health == nil || persisted.Health.LastState != "healthy" || persisted.Health.LastReason != "concurrent" {
		t.Fatalf("health = %+v, want concurrent health update preserved", persisted.Health)
	}
	if persisted.TickBackoff == nil || persisted.TickBackoff.LastFingerprint != "concurrent" {
		t.Fatalf("tick backoff = %+v, want concurrent tick update preserved", persisted.TickBackoff)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log: %v", err)
	}
	if strings.Contains(string(data), "kept setup") {
		t.Fatalf("cleanup log = %q, kept setup should have been skipped as already produced", string(data))
	}
}

func TestUp_StaleNodeCleanupFailurePreservesInspectableState(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	sessionName := "org/repo-13"
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{
				id:      "retired",
				scope:   "run",
				cleanup: fmt.Sprintf(`printf 'retired=%%s\n' '{{.Self.token}}' >> %s; exit 23`, cleanupLog),
			},
			{id: "kept", scope: "run", setup: "echo '{}'"},
		},
		[]nodeFixture{{id: "kept"}},
	)
	seedSession(t, store, sessionName, "org/repo", 13, "default", map[string]*contract.TaskState{
		"retired": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"token": "old-token"},
			Seq:     1,
		},
	})

	_, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err == nil {
		t.Fatal("expected up to fail when stale node cleanup fails")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrExecutionFailed {
		t.Fatalf("want ErrExecutionFailed, got %v", err)
	}
	persisted := store.Get(sessionName)
	if persisted == nil {
		t.Fatal("session must remain inspectable after cleanup failure")
	}
	st := persisted.Tasks["retired"]
	if st == nil || st.Status != contract.TaskStatusFailed || st.Outputs["token"] != "old-token" {
		t.Fatalf("retired task = %+v, want failed state with recorded outputs preserved", st)
	}
	if st := persisted.Tasks["kept"]; st != nil {
		t.Fatalf("kept task = %+v, want setup not attempted after stale cleanup failure", st)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log: %v", err)
	}
	if !strings.Contains(string(data), "retired=old-token\n") {
		t.Fatalf("cleanup log = %q, want failed cleanup to receive recorded output", string(data))
	}

	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "tasks", "retired.toml"), []byte(effectFixtureDoc(taskFixture{
		id:      "retired",
		scope:   "run",
		cleanup: fmt.Sprintf(`printf 'retired-again=%%s\n' '{{.Self.token}}' >> %s`, cleanupLog),
	})), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Up(cfg, store, UpParams{Identifier: sessionName})
	if err != nil {
		t.Fatalf("retry Up: %v", err)
	}
	if st := result.Tasks["retired"]; st != nil {
		t.Fatalf("retired task after retry = %+v, want dropped after successful cleanup", st)
	}
	if st := result.Tasks["kept"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("kept task after retry = %+v, want setup after stale cleanup succeeds", st)
	}
	data, err = os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log after retry: %v", err)
	}
	if !strings.Contains(string(data), "retired-again=old-token\n") {
		t.Fatalf("cleanup log = %q, want retry cleanup to receive recorded output", string(data))
	}
}

func TestUp_ForceRecreateResetsRuntimeWithoutPrev(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	oldWorkdirPath := filepath.Join(t.TempDir(), "old-workdir")
	newWorkdirPath := filepath.Join(t.TempDir(), "new-workdir")
	if err := os.MkdirAll(oldWorkdirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	cfg := writeWorkflowFixture(t, oldWorkdirPath, "default",
		[]taskFixture{
			{
				id:      "channel",
				scope:   "session",
				setup:   `printf '{"thread":"new-thread","prev":"%s"}' '{{get .Prev "thread" ""}}'`,
				cleanup: fmt.Sprintf(`printf 'channel=%%s\n' '{{.Self.thread}}' >> %s`, cleanupLog),
			},
			{
				id:      "runtime",
				scope:   "run",
				setup:   `printf '{"session_id":"new-runtime","prev":"%s"}' '{{get .Prev "session_id" ""}}'`,
				cleanup: fmt.Sprintf(`printf 'runtime=%%s\n' '{{.Self.session_id}}' >> %s`, cleanupLog),
			},
			{
				id:      "work",
				scope:   "session",
				cleanup: fmt.Sprintf(`printf 'work=%%s\n' '{{.Self.result}}' >> %s`, cleanupLog),
			},
		},
		[]nodeFixture{{id: "channel"}, {id: "runtime"}},
	)
	providersDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerSetup := fmt.Sprintf(`mkdir -p %s && printf '{"workspace_dir":%q,"branch":"new-branch","prev":"%%s"}' "$1"`, newWorkdirPath, newWorkdirPath)
	providerSetupArgs := `, { from = "prev.workspace_dir", default = "" }`
	providerCleanup := fmt.Sprintf(`printf 'workflow=%%s\n' "$1" >> %s; rm -rf "$1"`, cleanupLog)
	if err := os.WriteFile(filepath.Join(providersDir, "default_provider.toml"), []byte(providerScriptPair("default_provider", providerSetup, providerSetupArgs, providerCleanup, `, { from = "self.outputs.workspace_dir" }`)), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, "default", "workspace_provider = \"default_provider\"\n")
	sessionName := "org/repo-12"
	seedSession(t, store, "org/repo-parent", "org/repo", 11, "default", nil)
	seedSession(t, store, sessionName, "org/repo", 12, "default", map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{contract.OutputKeyWorkspaceDir: oldWorkdirPath, "branch": "old-branch"},
			Seq:     1,
		},
		"channel": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"thread": "old-thread"},
			Seq:     3,
		},
		"runtime": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"session_id": "old-runtime"},
			Seq:     4,
		},
		"work#1": {
			Scope:   contract.TaskScopeSession,
			TaskID:  "work",
			Status:  contract.TaskStatusProduced,
			Dynamic: true,
			Outputs: map[string]any{"result": "preserved"},
			Seq:     5,
		},
	})
	seedSession(t, store, "org/repo-child", "org/repo", 13, "default", nil)
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.ParentSession = "org/repo-parent"
		s.Alias = "resource-alias"
		s.WorkspaceDirPath = oldWorkdirPath
		s.Health = &contract.HealthState{LastCheckedAt: time.Now(), LastReason: "old"}
		s.LastTickAt = time.Now()
		s.TickBackoff = &contract.TickBackoff{LastFingerprint: "old"}
		return nil
	}); err != nil {
		t.Fatalf("update session: %v", err)
	}
	setParent(t, store, "org/repo-child", sessionName)
	logStore := eventlog.NewStore(store.Dir())
	_, _, next, err := logStore.Append(event.Event{SessionName: sessionName, Type: event.TypeUserEmit, Source: event.SourceCLI, Direction: event.Internal})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := logStore.CommitCursor(sessionName, "delivery", next); err != nil {
		t.Fatalf("commit cursor: %v", err)
	}

	result, err := Up(cfg, store, UpParams{Identifier: sessionName, ForceRecreate: true})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	workflow := result.Tasks[contract.WorkflowPseudoNodeID]
	if workflow == nil || workflow.Outputs[contract.OutputKeyWorkspaceDir] != newWorkdirPath || workflow.Outputs["prev"] != "" {
		t.Fatalf("workflow task = %+v, want provider setup rerun without Prev", workflow)
	}
	channel := result.Tasks["channel"]
	if channel == nil || channel.Outputs["thread"] != "new-thread" || channel.Outputs["prev"] != "" {
		t.Fatalf("channel task = %+v, want fresh setup without Prev", channel)
	}
	runtime := result.Tasks["runtime"]
	if runtime == nil || runtime.Outputs["session_id"] != "new-runtime" || runtime.Outputs["prev"] != "" {
		t.Fatalf("runtime task = %+v, want fresh setup without Prev", runtime)
	}
	if dynamic := result.Tasks["work#1"]; dynamic != nil {
		t.Fatalf("dynamic task = %+v, want removed", dynamic)
	}
	persisted := store.Get(sessionName)
	if persisted.ParentSession != "org/repo-parent" || len(persisted.Children) != 1 || persisted.Children[0] != "org/repo-child" {
		t.Fatalf("relations changed after force recreate: %+v", persisted)
	}
	if persisted.Alias != "resource-alias" || persisted.ResourceID != "https://github.com/org/repo/issues/12" {
		t.Fatalf("identity fields changed after force recreate: %+v", persisted)
	}
	if persisted.WorkspaceDirPath != newWorkdirPath {
		t.Fatalf("WorkspaceDirPath = %q, want recreated workdir %q", persisted.WorkspaceDirPath, newWorkdirPath)
	}
	if fileExists(oldWorkdirPath) {
		t.Fatalf("old workdir %q still exists", oldWorkdirPath)
	}
	if !fileExists(newWorkdirPath) {
		t.Fatalf("new workdir %q was not created", newWorkdirPath)
	}
	if domain.SessionBranch(persisted) != "new-branch" {
		t.Fatalf("Branch = %q, want provider output", domain.SessionBranch(persisted))
	}
	if persisted.Health != nil || !persisted.LastTickAt.IsZero() || persisted.TickBackoff != nil {
		t.Fatalf("runtime observation fields were not cleared: %+v", persisted)
	}
	evs, _, _, err := logStore.List(sessionName, 0, event.Filter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) == 0 || evs[0].Type != event.TypeUserEmit {
		t.Fatalf("event log not preserved: %+v", evs)
	}
	cursor, err := logStore.ReadCursor(sessionName, "delivery")
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != next {
		t.Fatalf("cursor = %d, want %d", cursor, next)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"runtime=old-runtime\n", "channel=old-thread\n", "work=preserved\n", "workflow=" + oldWorkdirPath + "\n"} {
		if !strings.Contains(log, want) {
			t.Fatalf("cleanup log = %q, missing %q", log, want)
		}
	}
}

func TestUp_ForceRecreateCleanupFailurePreservesInspectableState(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	oldWorkdirPath := filepath.Join(t.TempDir(), "old-workdir")
	if err := os.MkdirAll(oldWorkdirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	cfg := writeWorkflowFixture(t, oldWorkdirPath, "default",
		[]taskFixture{
			{
				id:      "channel",
				scope:   "session",
				setup:   `echo '{"thread":"old-thread"}'`,
				cleanup: fmt.Sprintf(`printf 'channel=%%s\n' '{{.Self.thread}}' >> %s`, cleanupLog),
			},
			{
				id:      "runtime",
				scope:   "run",
				setup:   `echo '{"session_id":"old-runtime"}'`,
				cleanup: fmt.Sprintf(`printf 'runtime=%%s\n' '{{.Self.session_id}}' >> %s; exit 23`, cleanupLog),
			},
		},
		[]nodeFixture{{id: "channel"}, {id: "runtime"}},
	)
	providersDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerSetup := `echo '{"workspace_dir":"/unused","branch":"unused"}'`
	providerSetupArgs := ""
	providerCleanup := fmt.Sprintf(`printf 'workflow=%%s\n' "$1" >> %s; rm -rf "$1"`, cleanupLog)
	if err := os.WriteFile(filepath.Join(providersDir, "default_provider.toml"), []byte(providerScriptPair("default_provider", providerSetup, providerSetupArgs, providerCleanup, `, { from = "self.outputs.workspace_dir" }`)), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, "default", "workspace_provider = \"default_provider\"\n")
	sessionName := "org/repo-15"
	seedSession(t, store, "org/repo-parent", "org/repo", 14, "default", nil)
	seedSession(t, store, sessionName, "org/repo", 15, "default", map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{contract.OutputKeyWorkspaceDir: oldWorkdirPath, "branch": "old-branch"},
			Seq:     1,
		},
		"channel": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"thread": "old-thread"},
			Seq:     3,
		},
		"runtime": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"session_id": "old-runtime"},
			Seq:     4,
		},
	})
	seedSession(t, store, "org/repo-child", "org/repo", 16, "default", nil)
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.ParentSession = "org/repo-parent"
		s.WorkspaceDirPath = oldWorkdirPath
		s.Health = &contract.HealthState{LastCheckedAt: time.Now(), LastReason: "old"}
		s.LastTickAt = time.Now()
		s.TickBackoff = &contract.TickBackoff{LastFingerprint: "old"}
		return nil
	}); err != nil {
		t.Fatalf("update session: %v", err)
	}
	setParent(t, store, "org/repo-child", sessionName)
	logStore := eventlog.NewStore(store.Dir())
	_, _, next, err := logStore.Append(event.Event{SessionName: sessionName, Type: event.TypeUserEmit, Source: event.SourceCLI, Direction: event.Internal})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := logStore.CommitCursor(sessionName, "delivery", next); err != nil {
		t.Fatalf("commit cursor: %v", err)
	}

	_, err = Up(cfg, store, UpParams{Identifier: sessionName, ForceRecreate: true})
	if err == nil {
		t.Fatal("expected force recreate to fail when task cleanup fails")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrExecutionFailed {
		t.Fatalf("want ErrExecutionFailed, got %v", err)
	}

	persisted := store.Get(sessionName)
	if persisted == nil {
		t.Fatal("session must remain inspectable after cleanup failure")
	}
	if persisted.ParentSession != "org/repo-parent" || len(persisted.Children) != 1 || persisted.Children[0] != "org/repo-child" {
		t.Fatalf("relations changed after failed force recreate: %+v", persisted)
	}
	if persisted.ResourceID != "https://github.com/org/repo/issues/15" {
		t.Fatalf("ResourceID = %q, want preserved binding", persisted.ResourceID)
	}
	if persisted.WorkspaceDirPath != oldWorkdirPath || domain.SessionBranch(persisted) != "old-branch" {
		t.Fatalf("runtime session state = (%q, %q), want preserved before reset", persisted.WorkspaceDirPath, domain.SessionBranch(persisted))
	}
	if persisted.Health == nil || persisted.LastTickAt.IsZero() || persisted.TickBackoff == nil {
		t.Fatalf("runtime observation fields were reset after cleanup failure: %+v", persisted)
	}
	if !fileExists(oldWorkdirPath) {
		t.Fatalf("old workdir %q was removed before workflow cleanup", oldWorkdirPath)
	}
	workflow := persisted.Tasks[contract.WorkflowPseudoNodeID]
	if workflow == nil || workflow.Status != contract.TaskStatusProduced || workflow.Outputs[contract.OutputKeyWorkspaceDir] != oldWorkdirPath {
		t.Fatalf("@workflow state = %+v, want preserved produced state", workflow)
	}
	runtime := persisted.Tasks["runtime"]
	if runtime == nil || runtime.Status != contract.TaskStatusFailed || runtime.Outputs["session_id"] != "old-runtime" {
		t.Fatalf("runtime task = %+v, want failed cleanup state with prior outputs", runtime)
	}
	channel := persisted.Tasks["channel"]
	if channel == nil || channel.Status != contract.TaskStatusCleaned || channel.Outputs["thread"] != "old-thread" {
		t.Fatalf("channel task = %+v, want later cleanup result persisted", channel)
	}
	cursor, err := logStore.ReadCursor(sessionName, "delivery")
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != next {
		t.Fatalf("cursor = %d, want %d", cursor, next)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"runtime=old-runtime\n", "channel=old-thread\n"} {
		if !strings.Contains(log, want) {
			t.Fatalf("cleanup log = %q, missing %q", log, want)
		}
	}
	for _, notWant := range []string{"workflow=" + oldWorkdirPath + "\n"} {
		if strings.Contains(log, notWant) {
			t.Fatalf("cleanup log = %q, should not contain %q", log, notWant)
		}
	}
}

func TestUp_ForceRecreateProviderSetupFailurePersistsInspectableState(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	oldWorkdirPath := filepath.Join(t.TempDir(), "old-workdir")
	if err := os.MkdirAll(oldWorkdirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
	cfg := writeWorkflowFixture(t, oldWorkdirPath, "default",
		[]taskFixture{{
			id:      "runtime",
			scope:   "run",
			setup:   `echo '{"session_id":"old-runtime"}'`,
			cleanup: fmt.Sprintf(`printf 'runtime=%%s\n' '{{.Self.session_id}}' >> %s`, cleanupLog),
		}},
		[]nodeFixture{{id: "runtime"}},
	)
	providersDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerSetup := `printf 'provider setup failed\n' >&2; exit 42`
	providerSetupArgs := ""
	providerCleanup := fmt.Sprintf(`printf 'workflow=%%s\n' "$1" >> %s; rm -rf "$1"`, cleanupLog)
	if err := os.WriteFile(filepath.Join(providersDir, "default_provider.toml"), []byte(providerScriptPair("default_provider", providerSetup, providerSetupArgs, providerCleanup, `, { from = "self.outputs.workspace_dir" }`)), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, "default", "workspace_provider = \"default_provider\"\n")
	sessionName := "org/repo-13"
	seedSession(t, store, "org/repo-parent", "org/repo", 12, "default", nil)
	seedSession(t, store, sessionName, "org/repo", 13, "default", map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{contract.OutputKeyWorkspaceDir: oldWorkdirPath, "branch": "old-branch"},
			Seq:     1,
		},
		"runtime": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"session_id": "old-runtime"},
			Seq:     2,
		},
	})
	seedSession(t, store, "org/repo-child", "org/repo", 14, "default", nil)
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.ParentSession = "org/repo-parent"
		s.WorkspaceDirPath = oldWorkdirPath
		return nil
	}); err != nil {
		t.Fatalf("update session: %v", err)
	}
	setParent(t, store, "org/repo-child", sessionName)
	logStore := eventlog.NewStore(store.Dir())
	_, _, next, err := logStore.Append(event.Event{SessionName: sessionName, Type: event.TypeUserEmit, Source: event.SourceCLI, Direction: event.Internal})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := logStore.CommitCursor(sessionName, "delivery", next); err != nil {
		t.Fatalf("commit cursor: %v", err)
	}

	_, err = Up(cfg, store, UpParams{Identifier: sessionName, ForceRecreate: true})
	if err == nil {
		t.Fatal("expected force recreate to fail when provider setup fails")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrExecutionFailed {
		t.Fatalf("want ErrExecutionFailed, got %v", err)
	}

	persisted := store.Get(sessionName)
	if persisted == nil {
		t.Fatal("session must remain inspectable after provider setup failure")
	}
	if persisted.ParentSession != "org/repo-parent" || len(persisted.Children) != 1 || persisted.Children[0] != "org/repo-child" {
		t.Fatalf("relations changed after failed force recreate: %+v", persisted)
	}
	if persisted.ResourceID != "https://github.com/org/repo/issues/13" {
		t.Fatalf("ResourceID = %q, want preserved binding", persisted.ResourceID)
	}
	if persisted.WorkspaceDirPath != "" || domain.SessionBranch(persisted) != "" {
		t.Fatalf("runtime session state = (%q, %q), want cleared after cleanup", persisted.WorkspaceDirPath, domain.SessionBranch(persisted))
	}
	workflow := persisted.Tasks[contract.WorkflowPseudoNodeID]
	if workflow == nil || workflow.Status != contract.TaskStatusFailed {
		t.Fatalf("@workflow state = %+v, want failed state persisted", workflow)
	}
	if workflow.Outputs != nil {
		t.Fatalf("@workflow outputs = %+v, want no Prev after reset", workflow.Outputs)
	}
	if persisted.Tasks["runtime"] != nil {
		t.Fatalf("runtime task = %+v, want cleared after reset", persisted.Tasks["runtime"])
	}
	if fileExists(oldWorkdirPath) {
		t.Fatalf("old workdir %q still exists", oldWorkdirPath)
	}
	cursor, err := logStore.ReadCursor(sessionName, "delivery")
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != next {
		t.Fatalf("cursor = %d, want %d", cursor, next)
	}
	data, err := os.ReadFile(cleanupLog)
	if err != nil {
		t.Fatalf("read cleanup log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"runtime=old-runtime\n", "workflow=" + oldWorkdirPath + "\n"} {
		if !strings.Contains(log, want) {
			t.Fatalf("cleanup log = %q, missing %q", log, want)
		}
	}
}

func TestRecreateSessionRuntimeTeardownListFailurePreservesRuntimeFieldsButMarksStatusDown(t *testing.T) {
	store := testStore(t)
	oldWorkdirPath := filepath.Join(t.TempDir(), "old-workdir")
	if err := os.MkdirAll(oldWorkdirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := writeWorkflowFixture(t, oldWorkdirPath, "default",
		[]taskFixture{{id: "runtime", scope: "run", cleanup: "true"}},
		[]nodeFixture{{id: "runtime"}},
	)
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "tasks", "broken.toml"), []byte("scope = \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionName := "org/repo-20"
	seedSession(t, store, sessionName, "org/repo", 20, "default", map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
		"runtime": {
			Scope:   contract.TaskScopeRun,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"session_id": "old-runtime"},
			Seq:     1,
		},
	})
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.Status = contract.SessionStatusUp
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	session := store.Get(sessionName)
	session.WorkspaceDirPath = oldWorkdirPath
	session.Tasks[contract.WorkflowPseudoNodeID].Outputs["branch"] = "old-branch"

	_, err := recreateSessionRuntime(cfg, store, sessionName, session, config.WorkflowFile{ID: "default"}, &taskpkg.Plan{
		Run: []taskpkg.Resolved{{
			NodeID:  "runtime",
			TaskID:  "runtime",
			Scope:   contract.TaskScopeRun,
			Cleanup: &lang.Action{Type: lang.ActionShell, Script: "true"},
		}},
	}, nil)
	if err == nil {
		t.Fatal("expected force recreate to fail when teardown list construction fails")
	}
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrExecutionFailed {
		t.Fatalf("want ErrExecutionFailed, got %v", err)
	}

	persisted := store.Get(sessionName)
	if persisted == nil {
		t.Fatal("session must remain inspectable after teardown list failure")
	}
	if persisted.WorkspaceDirPath != "" || domain.SessionBranch(persisted) != "issue/1" {
		t.Fatalf("persisted session state = (%q, %q), want original stored values", persisted.WorkspaceDirPath, domain.SessionBranch(persisted))
	}
	runtime := persisted.Tasks["runtime"]
	if runtime == nil || runtime.Status != contract.TaskStatusProduced || runtime.Outputs["session_id"] != "old-runtime" {
		t.Fatalf("runtime task = %+v, want untouched produced state", runtime)
	}
	if persisted.Status != contract.SessionStatusDown {
		t.Fatalf("Status = %q, want %q (force-recreate marks it down before its own teardown even runs)", persisted.Status, contract.SessionStatusDown)
	}
}

func TestUp_ForceRecreateFailureStagesPersistInspectableState(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	type expectations struct {
		workdirPath string
		branch      string
		oldExists   bool
		newExists   bool
		tasks       map[string]string
		absentTasks []string
	}
	type failureCase struct {
		name       string
		sessionNum int
		configure  func(t *testing.T, cfg *config.Config, oldWorkdirPath, newWorkdirPath, cleanupLog string)
		want       func(oldWorkdirPath, newWorkdirPath string) expectations
	}

	cases := []failureCase{
		{
			name:       "workflow cleanup failure stops before reset",
			sessionNum: 22,
			configure: func(t *testing.T, cfg *config.Config, oldWorkdirPath, newWorkdirPath, cleanupLog string) {
				writeForceRecreateProvider(t, cfg,
					fmt.Sprintf(`mkdir -p %s && printf '{"workspace_dir":%q,"branch":"new-branch"}'`, newWorkdirPath, newWorkdirPath),
					fmt.Sprintf(`printf 'workflow=%%s\n' "$1" >> %s; exit 25`, cleanupLog),
				)
				prependForceRecreateWorkflow(t, cfg, "workspace_provider = \"default_provider\"\n")
			},
			want: func(oldWorkdirPath, newWorkdirPath string) expectations {
				return expectations{
					workdirPath: oldWorkdirPath,
					branch:      "old-branch",
					oldExists:   true,
					tasks: map[string]string{
						contract.WorkflowPseudoNodeID: contract.TaskStatusFailed,
						"runtime":                     contract.TaskStatusCleaned,
						"channel":                     contract.TaskStatusCleaned,
					},
				}
			},
		},
		{
			name:       "workflow reload failure persists reset session state",
			sessionNum: 23,
			configure: func(t *testing.T, cfg *config.Config, oldWorkdirPath, newWorkdirPath, cleanupLog string) {
				wfPath := filepath.Join(cfg.BaseDir, "workflows", "default.toml")
				writeForceRecreateProvider(t, cfg,
					fmt.Sprintf(`mkdir -p %s && rm -f %s && printf '{"workspace_dir":%q,"branch":"new-branch"}'`, newWorkdirPath, wfPath, newWorkdirPath),
					fmt.Sprintf(`printf 'workflow=%%s\n' "$1" >> %s; rm -rf "$1"`, cleanupLog),
				)
				prependForceRecreateWorkflow(t, cfg, "workspace_provider = \"default_provider\"\n")
			},
			want: func(oldWorkdirPath, newWorkdirPath string) expectations {
				return expectations{
					workdirPath: newWorkdirPath,
					branch:      "new-branch",
					oldExists:   false,
					newExists:   true,
					tasks: map[string]string{
						contract.WorkflowPseudoNodeID: contract.TaskStatusProduced,
					},
					absentTasks: []string{"runtime", "channel"},
				}
			},
		},
		{
			name:       "plan build failure persists reset session state",
			sessionNum: 24,
			configure: func(t *testing.T, cfg *config.Config, oldWorkdirPath, newWorkdirPath, cleanupLog string) {
				taskPath := filepath.Join(cfg.BaseDir, "tasks", "runtime.toml")
				writeForceRecreateProvider(t, cfg,
					fmt.Sprintf(`mkdir -p %s && rm -f %s && printf '{"workspace_dir":%q,"branch":"new-branch"}'`, newWorkdirPath, taskPath, newWorkdirPath),
					fmt.Sprintf(`printf 'workflow=%%s\n' "$1" >> %s; rm -rf "$1"`, cleanupLog),
				)
				prependForceRecreateWorkflow(t, cfg, "workspace_provider = \"default_provider\"\n")
			},
			want: func(oldWorkdirPath, newWorkdirPath string) expectations {
				return expectations{
					workdirPath: newWorkdirPath,
					branch:      "new-branch",
					oldExists:   false,
					newExists:   true,
					tasks: map[string]string{
						contract.WorkflowPseudoNodeID: contract.TaskStatusProduced,
					},
					absentTasks: []string{"runtime", "channel"},
				}
			},
		},
		{
			name:       "session setup failure persists failed task state",
			sessionNum: 27,
			configure: func(t *testing.T, cfg *config.Config, oldWorkdirPath, newWorkdirPath, cleanupLog string) {
				writeForceRecreateProvider(t, cfg,
					fmt.Sprintf(`mkdir -p %s && printf '{"workspace_dir":%q,"branch":"new-branch"}'`, newWorkdirPath, newWorkdirPath),
					fmt.Sprintf(`printf 'workflow=%%s\n' "$1" >> %s; rm -rf "$1"`, cleanupLog),
				)
				prependForceRecreateWorkflow(t, cfg, "workspace_provider = \"default_provider\"\n")
				writeTaskFixture(t, cfg, taskFixture{
					id:      "channel",
					scope:   "session",
					setup:   `printf 'channel setup failed\n' >&2; exit 27`,
					cleanup: fmt.Sprintf(`printf 'channel=%%s\n' '{{.Self.thread}}' >> %s`, cleanupLog),
				})
			},
			want: func(oldWorkdirPath, newWorkdirPath string) expectations {
				return expectations{
					workdirPath: newWorkdirPath,
					branch:      "new-branch",
					oldExists:   false,
					newExists:   true,
					tasks: map[string]string{
						contract.WorkflowPseudoNodeID: contract.TaskStatusProduced,
						"channel":                     contract.TaskStatusFailed,
					},
					absentTasks: []string{"runtime"},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			oldWorkdirPath := filepath.Join(t.TempDir(), "old-workdir")
			newWorkdirPath := filepath.Join(t.TempDir(), "new-workdir")
			if err := os.MkdirAll(oldWorkdirPath, 0o755); err != nil {
				t.Fatal(err)
			}
			cleanupLog := filepath.Join(t.TempDir(), "cleanup.log")
			cfg := writeWorkflowFixture(t, oldWorkdirPath, "default",
				[]taskFixture{
					{
						id:      "channel",
						scope:   "session",
						setup:   `printf '{"thread":"new-thread","prev":"%s"}' '{{get .Prev "thread" ""}}'`,
						cleanup: fmt.Sprintf(`printf 'channel=%%s\n' '{{.Self.thread}}' >> %s`, cleanupLog),
					},
					{
						id:      "runtime",
						scope:   "run",
						setup:   `printf '{"session_id":"new-runtime","prev":"%s"}' '{{get .Prev "session_id" ""}}'`,
						cleanup: fmt.Sprintf(`printf 'runtime=%%s\n' '{{.Self.session_id}}' >> %s`, cleanupLog),
					},
				},
				[]nodeFixture{{id: "channel"}, {id: "runtime"}},
			)
			tc.configure(t, cfg, oldWorkdirPath, newWorkdirPath, cleanupLog)

			sessionName := fmt.Sprintf("org/repo-%d", tc.sessionNum)
			seedSession(t, store, "org/repo-parent", "org/repo", tc.sessionNum-1, "default", nil)
			seedSession(t, store, sessionName, "org/repo", tc.sessionNum, "default", map[string]*contract.TaskState{
				contract.WorkflowPseudoNodeID: {
					Scope:   contract.TaskScopeSession,
					Status:  contract.TaskStatusProduced,
					Outputs: map[string]any{contract.OutputKeyWorkspaceDir: oldWorkdirPath, "branch": "old-branch"},
					Seq:     1,
				},
				"channel": {
					Scope:   contract.TaskScopeSession,
					Status:  contract.TaskStatusProduced,
					Outputs: map[string]any{"thread": "old-thread"},
					Seq:     3,
				},
				"runtime": {
					Scope:   contract.TaskScopeRun,
					Status:  contract.TaskStatusProduced,
					Outputs: map[string]any{"session_id": "old-runtime"},
					Seq:     4,
				},
			})
			seedSession(t, store, "org/repo-child", "org/repo", tc.sessionNum+1, "default", nil)
			if err := store.Update(sessionName, func(s *domain.Session) error {
				s.ParentSession = "org/repo-parent"
				s.WorkspaceDirPath = oldWorkdirPath
				return nil
			}); err != nil {
				t.Fatalf("update session: %v", err)
			}
			setParent(t, store, "org/repo-child", sessionName)
			logStore := eventlog.NewStore(store.Dir())
			_, _, next, err := logStore.Append(event.Event{SessionName: sessionName, Type: event.TypeUserEmit, Source: event.SourceCLI, Direction: event.Internal})
			if err != nil {
				t.Fatalf("append event: %v", err)
			}
			if err := logStore.CommitCursor(sessionName, "delivery", next); err != nil {
				t.Fatalf("commit cursor: %v", err)
			}

			_, err = Up(cfg, store, UpParams{Identifier: sessionName, ForceRecreate: true})
			if err == nil {
				t.Fatal("expected force recreate to fail")
			}
			svcErr, ok := err.(*Error)
			if !ok || svcErr.Code != ErrExecutionFailed {
				t.Fatalf("want ErrExecutionFailed, got %v", err)
			}

			persisted := store.Get(sessionName)
			if persisted == nil {
				t.Fatal("session must remain inspectable after force recreate failure")
			}
			if persisted.ParentSession != "org/repo-parent" || len(persisted.Children) != 1 || persisted.Children[0] != "org/repo-child" {
				t.Fatalf("relations changed after failed force recreate: %+v", persisted)
			}
			if persisted.ResourceID != fmt.Sprintf("https://github.com/org/repo/issues/%d", tc.sessionNum) {
				t.Fatalf("ResourceID = %q, want preserved binding", persisted.ResourceID)
			}
			want := tc.want(oldWorkdirPath, newWorkdirPath)
			if persisted.WorkspaceDirPath != want.workdirPath || domain.SessionBranch(persisted) != want.branch {
				t.Fatalf("runtime session state = (%q, %q), want (%q, %q)", persisted.WorkspaceDirPath, domain.SessionBranch(persisted), want.workdirPath, want.branch)
			}
			if fileExists(oldWorkdirPath) != want.oldExists {
				t.Fatalf("old workdir exists = %v, want %v", fileExists(oldWorkdirPath), want.oldExists)
			}
			if fileExists(newWorkdirPath) != want.newExists {
				t.Fatalf("new workdir exists = %v, want %v", fileExists(newWorkdirPath), want.newExists)
			}
			for taskName, status := range want.tasks {
				st := persisted.Tasks[taskName]
				if st == nil || st.Status != status {
					t.Fatalf("task %q = %+v, want status %q", taskName, st, status)
				}
			}
			for _, taskName := range want.absentTasks {
				if st := persisted.Tasks[taskName]; st != nil {
					t.Fatalf("task %q = %+v, want absent", taskName, st)
				}
			}
			cursor, err := logStore.ReadCursor(sessionName, "delivery")
			if err != nil {
				t.Fatalf("read cursor: %v", err)
			}
			if cursor != next {
				t.Fatalf("cursor = %d, want %d", cursor, next)
			}
		})
	}
}

func writeForceRecreateProvider(t *testing.T, cfg *config.Config, setup, cleanup string) {
	t.Helper()
	providersDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := providerScriptPair("default_provider", setup, "", cleanup, `, { from = "self.outputs.workspace_dir" }`)
	if err := os.WriteFile(filepath.Join(providersDir, "default_provider.toml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func prependForceRecreateWorkflow(t *testing.T, cfg *config.Config, fields string) {
	t.Helper()
	addWorkflowFields(t, cfg, "default", fields)
}

// writeTaskFixture renders one effect declaration: the definition table, its
// lifecycle actions as shell actions, and whatever else the case declares
// under that table. A fixture's `extra` is written as the definition's own
// body — bare keys on the definition table, tables under it — so a case
// states only the fields it is about.
func writeTaskFixture(t *testing.T, cfg *config.Config, fixture taskFixture) {
	t.Helper()
	bare, tables := splitFixtureExtra(fixture.id, fixture.extra)
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\nkind = \"effect\"\n", fixture.id)
	if fixture.scope != "" {
		fmt.Fprintf(&b, "scope = %q\n", fixture.scope)
	}
	b.WriteString(bare)
	for _, hook := range []struct {
		name   string
		script string
	}{{"setup", fixture.setup}, {"cleanup", fixture.cleanup}} {
		if hook.script == "" {
			continue
		}
		b.WriteString(shellFixtureAction(fixture.id, hook.name, hook.script))
	}
	if fixture.setup != "" {
		// Every setup-bearing effect must declare [health.alive]; this fixture
		// is not about liveness, so it gets the noop filler.
		fmt.Fprintf(&b, "\n[%s.health.alive]\ntype = \"noop\"\n", fixture.id)
	}
	b.WriteString(tables)
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "tasks", fixture.id+".toml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// splitFixtureExtra separates a fixture's extra body into the definition
// table's own keys and the tables below it, re-anchoring each table under the
// definition id. `inner` becomes the joint's own table, since the joint names
// what it wraps through `uses`.
func splitFixtureExtra(id, extra string) (bare, tables string) {
	var bareLines, tableLines []string
	inTable := false
	for _, line := range strings.Split(extra, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "[["):
			inTable = true
			tableLines = append(tableLines, "[["+id+"."+strings.TrimPrefix(trimmed, "[["))
		case strings.HasPrefix(trimmed, "["):
			inTable = true
			tableLines = append(tableLines, "["+id+"."+strings.TrimPrefix(trimmed, "["))
		case inTable:
			tableLines = append(tableLines, line)
		case strings.HasPrefix(trimmed, "inner ="):
			tableLines = append(tableLines, "", "["+id+".inner]", "uses ="+strings.TrimPrefix(trimmed, "inner ="))
			inTable = true
		case trimmed == "":
		default:
			bareLines = append(bareLines, trimmed)
		}
	}
	if len(bareLines) > 0 {
		bare = strings.Join(bareLines, "\n") + "\n"
	}
	if len(tableLines) > 0 {
		tables = "\n" + strings.Join(tableLines, "\n") + "\n"
	}
	return bare, tables
}

func TestDestroy_WritesTombstone(t *testing.T) {
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
	if store.Get(sessionName) != nil {
		t.Fatal("expected state entry deleted after destroy")
	}

	data, ok, err := eventlog.NewStore(store.Dir()).ReadTombstone(sessionName)
	if err != nil {
		t.Fatalf("ReadTombstone: %v", err)
	}
	if !ok {
		t.Fatal("expected a tombstone to exist after destroy")
	}
	var tomb contract.Tombstone
	if err := json.Unmarshal(data, &tomb); err != nil {
		t.Fatalf("unmarshal tombstone: %v", err)
	}
	if tomb.Name != sessionName {
		t.Errorf("tombstone.Name = %q, want %q", tomb.Name, sessionName)
	}
	if tomb.DestroyedAt.IsZero() {
		t.Error("expected DestroyedAt to be set")
	}
	st := tomb.Tasks["envfile"]
	if st == nil || st.Outputs["path"] != "/tmp/env" {
		t.Errorf("expected envfile outputs preserved in tombstone, got %+v", tomb.Tasks)
	}
}

func TestDestroy_DefaultFailsFastOnRunCleanupError(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{
			{id: "tmux", scope: "run", setup: "true", cleanup: "exit 1"},
			{id: "envfile", scope: "session", setup: "true", cleanup: "true"},
		},
		[]nodeFixture{{id: "tmux"}, {id: "envfile"}},
	)
	sessionName := "org/repo-1"
	seedSession(t, store, sessionName, "org/repo", 1, "default", map[string]*contract.TaskState{
		"tmux":    {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
		"envfile": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	_, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName})
	if err == nil {
		t.Fatal("expected error without --force")
	}
	if got := store.Get(sessionName); got == nil {
		t.Fatal("expected state entry preserved for retry, was deleted")
	}
}

// TestDestroy_ForceContinuesOnCleanupError exercises the --force policy:
// cleanup errors are recorded as warnings, session-scoped cleanup still runs
// after a run-scoped failure, and the state entry is deleted.
func TestDestroy_ForceContinuesOnCleanupError(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	tmpDir := t.TempDir()
	marker := tmpDir + "/session-cleanup-ran"
	cfg := writeWorkflowFixture(t, tmpDir, "default",
		[]taskFixture{
			{id: "tmux", scope: "run", setup: "true", cleanup: "exit 1"},
			{id: "envfile", scope: "session", setup: "true", cleanup: "touch " + marker},
		},
		[]nodeFixture{{id: "tmux"}, {id: "envfile"}},
	)
	sessionName := "org/repo-2"
	seedSession(t, store, sessionName, "org/repo", 2, "default", map[string]*contract.TaskState{
		"tmux":    {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
		"envfile": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	result, err := Destroy(cfg, store, DestroyParams{Identifier: sessionName, Force: true})
	if err != nil {
		t.Fatalf("Destroy --force returned error, want nil: %v", err)
	}
	if len(result.CleanupWarnings) == 0 {
		t.Fatal("expected CleanupWarnings to record run cleanup failure")
	}
	// The teardown is a single reverse-instantiation pass, so the
	// warning is prefixed "cleanup:" rather than the old per-scope label; the
	// failing task (tmux) is named in it.
	if joined := strings.Join(result.CleanupWarnings, "|"); !strings.Contains(joined, "tmux") {
		t.Fatalf("expected tmux cleanup failure in warnings, got %q", joined)
	}
	if _, err := exec.Command("bash", "-c", "test -f "+marker).CombinedOutput(); err != nil {
		t.Fatalf("expected session cleanup marker at %s, missing", marker)
	}
	if got := store.Get(sessionName); got != nil {
		t.Fatalf("expected state entry deleted, still present: %+v", got)
	}
}

// TestDestroy_BlocksWhenChildrenExist covers the fail-closed guard:
// store.Delete unconditionally clears a child's ParentSession, so destroying
// a session with children must abort before any teardown step runs.
func TestDestroy_BlocksWhenChildrenExist(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	parentName := "org/repo-1"
	childName := "org/repo-2"
	seedSession(t, store, parentName, "org/repo", 1, "default", nil)
	seedSession(t, store, childName, "org/repo", 2, "default", nil)
	setParent(t, store, childName, parentName)

	_, err := Destroy(cfg, store, DestroyParams{Identifier: parentName})
	svcErr, ok := err.(*Error)
	if !ok || svcErr.Code != ErrHasChildren {
		t.Fatalf("want ErrHasChildren, got %v", err)
	}
	if !strings.Contains(svcErr.Message, childName) {
		t.Errorf("expected child session name in error message, got %q", svcErr.Message)
	}
	if store.Get(parentName) == nil {
		t.Fatal("rejected destroy must leave the parent session intact")
	}
	if got := store.Get(childName); got == nil || got.ParentSession != parentName {
		t.Fatal("rejected destroy must leave the child's ParentSession intact")
	}
}

// TestDestroy_FailsClosedWhenStoreUnreadable proves Destroy never proceeds
// past its own children check when the store cannot be read at all —
// store.Delete unconditionally clears a child's ParentSession, so a
// swallowed read error returning an empty (so apparently childless) session
// map would let Destroy silently orphan real children instead of aborting.
func TestDestroy_FailsClosedWhenStoreUnreadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(persistence.PathIn(dir), []byte("not a database"), 0644); err != nil {
		t.Fatal(err)
	}
	store := state.NewStore(dir)
	cfg := &config.Config{}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "org/repo-1"}); err == nil {
		t.Fatal("Destroy over an unreadable store must fail, not silently proceed")
	}
}

// TestDestroy_ForceKeepsChildrensParentLinkWithWarning: --force warns about
// the child but keeps its ParentSession naming the retained parent.
func TestDestroy_ForceKeepsChildrensParentLinkWithWarning(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "envfile", scope: "session", setup: "true", cleanup: "true"}},
		[]nodeFixture{{id: "envfile"}},
	)
	parentName := "org/repo-1"
	childName := "org/repo-2"
	seedSession(t, store, parentName, "org/repo", 1, "default", map[string]*contract.TaskState{
		"envfile": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})
	seedSession(t, store, childName, "org/repo", 2, "default", nil)
	setParent(t, store, childName, parentName)

	result, err := Destroy(cfg, store, DestroyParams{Identifier: parentName, Force: true})
	if err != nil {
		t.Fatalf("Destroy --force returned error, want nil: %v", err)
	}
	if joined := strings.Join(result.CleanupWarnings, "|"); !strings.Contains(joined, childName) {
		t.Fatalf("expected orphaned child %q in CleanupWarnings, got %q", childName, joined)
	}
	if store.Get(parentName) != nil {
		t.Fatal("expected parent state entry deleted")
	}
	got := store.Get(childName)
	if got == nil {
		t.Fatal("expected child session to still exist (orphaned, not deleted)")
	}
	if got.ParentSession != parentName {
		t.Errorf("expected child ParentSession to keep naming destroyed parent %q, got %q", parentName, got.ParentSession)
	}
}

// TestUp_ForceRecreateRendersTerminalHelperAgainstThisPassOutputs is the
// end-to-end shape of the reported failure: on a recreate the whole task map
// is reset, so a downstream node's {{terminal "..."}} has to resolve against
// the [terminal]-declaring node's outputs from the very pass it is running
// in, not from the emptied state the pass started with.
func TestUp_ForceRecreateRendersTerminalHelperAgainstThisPassOutputs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	store := testStore(t)
	oldWorkdirPath := filepath.Join(t.TempDir(), "old-workdir")
	newWorkdirPath := filepath.Join(t.TempDir(), "new-workdir")
	if err := os.MkdirAll(oldWorkdirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := writeWorkflowFixture(t, oldWorkdirPath, "default",
		[]taskFixture{
			{
				id:       "terminal",
				scope:    "session",
				setup:    `echo '{"session_name":"agent-recreated"}'`,
				sendText: `printf 'sent=%s' '{{.Self.session_name}}'`,
			},
			{
				id:    "prompt",
				scope: "session",
				setup: `printf '{"delivered":"%s"}' "$(sh -c "{{terminal "send_text"}}" send-text)"`,
			},
		},
		[]nodeFixture{
			{id: "terminal"},
			{id: "prompt", inputs: map[string]*lang.Value{"session": fromValue("nodes.terminal.outputs.session_name")}},
		},
	)
	providersDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	providerSetup := fmt.Sprintf(`mkdir -p %s && printf '{"workspace_dir":%q}'`, newWorkdirPath, newWorkdirPath)
	if err := os.WriteFile(filepath.Join(providersDir, "default_provider.toml"),
		[]byte(providerScriptPair("default_provider", providerSetup, "", "true", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, "default", "workspace_provider = \"default_provider\"\n")
	sessionName := "org/repo-14"
	seedSession(t, store, sessionName, "org/repo", 14, "default", map[string]*contract.TaskState{
		contract.WorkflowPseudoNodeID: {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{contract.OutputKeyWorkspaceDir: oldWorkdirPath},
			Seq:     1,
		},
		"terminal": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"session_name": "agent-old"},
			Seq:     2,
		},
		"prompt": {
			Scope:   contract.TaskScopeSession,
			Status:  contract.TaskStatusProduced,
			Outputs: map[string]any{"delivered": "sent=agent-old"},
			Seq:     3,
		},
	})
	if err := store.Update(sessionName, func(s *domain.Session) error {
		s.WorkspaceDirPath = oldWorkdirPath
		return nil
	}); err != nil {
		t.Fatalf("update session: %v", err)
	}

	result, err := Up(cfg, store, UpParams{Identifier: sessionName, ForceRecreate: true})
	if err != nil {
		t.Fatalf("Up --force-recreate: %v", err)
	}
	prompt := result.Tasks["prompt"]
	if prompt == nil || prompt.Outputs["delivered"] != "sent=agent-recreated" {
		t.Fatalf("prompt task = %+v, want the terminal verb rendered against this pass's session_name", prompt)
	}
}

// providerScriptPair is a resolver-less provider whose setup and cleanup each
// run one literal script, for a fixture whose subject is the lifecycle around
// them.
func providerScriptPair(id, setup, setupArgs, cleanup, cleanupArgs string) string {
	return fmt.Sprintf(`
[%[1]s]
kind = "workspace_provider"

[%[1]s.setup]
type    = "exec"
command = "sh"
args    = ["-c", %[2]q, "provider"%[3]s]

[%[1]s.cleanup]
type    = "exec"
command = "sh"
args    = ["-c", %[4]q, "provider"%[5]s]

# A cleanup reading an output has to declare it: the contract is what makes
# the read resolvable at load.
[%[1]s.outputs_schema]
type = "object"

[%[1]s.outputs_schema.properties]
workspace_dir = { type = "string" }
branch        = { type = "string" }
`, id, setup, setupArgs, cleanup, cleanupArgs)
}
