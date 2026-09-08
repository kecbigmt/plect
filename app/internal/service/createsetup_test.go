package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/testpath"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// writeSetupWorkflow gives the fixture workflow a workspace provider: the
// extra TOML lands in workspaces/<wfID>.toml and the workflow gains
// `workspace_provider = "<wfID>"`.
func writeSetupWorkflow(t *testing.T, cfg *config.Config, wfID, extra string) {
	t.Helper()
	workspacesDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(workspacesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	provID, body := providerAside(wfID, extra)
	if err := os.WriteFile(filepath.Join(workspacesDir, provID+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	addWorkflowFields(t, cfg, wfID, "workspace_provider = \""+provID+"\"\n")
}

func TestCreate_WorkflowSetupPath(t *testing.T) {
	store := testStore(t)
	// A subprocess's $(pwd) reports the kernel's resolved cwd (/private/var/...
	// on darwin), so workdir must be resolved the same way before comparing.
	workdir := filepath.Join(testpath.Real(t, t.TempDir()), "wd")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "probe", scope: "session", setup: `echo "{\"cwd\":\"$(pwd)\"}"`}},
		[]nodeFixture{{id: "probe"}})
	writeSetupWorkflow(t, cfg, "wf", providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s","branch":"issue/5","title":"T"}'
`, workdir, workdir)))

	result, err := Create(cfg, store, CreateParams{URL: "https://github.com/org/repo/issues/5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.WorkspaceDirPath != workdir {
		t.Errorf("WorkspaceDirPath = %q, want %q (mirrored from workdir output)", result.WorkspaceDirPath, workdir)
	}
	if result.Branch != "issue/5" {
		t.Errorf("Branch = %q, want issue/5", result.Branch)
	}

	s := store.Get("org/repo-5+wf")
	if s == nil {
		t.Fatal("session not persisted")
	}
	wfState := s.Nodes[contract.WorkflowPseudoNodeID]
	if wfState == nil || wfState.Status != contract.TaskStatusProduced {
		t.Fatalf("pseudo-node = %+v, want produced", wfState)
	}
	if wfState.Outputs["title"] != "T" {
		t.Errorf("free-form outputs should persist: %v", wfState.Outputs)
	}
	// Session tasks ran with the new workdir as cwd.
	probe := s.Nodes["probe"]
	if probe == nil || probe.Status != contract.TaskStatusProduced {
		t.Fatalf("probe = %+v", probe)
	}
	if got, _ := probe.Outputs["cwd"].(string); got != workdir {
		t.Errorf("task cwd = %q, want workdir %q", got, workdir)
	}
}

func TestCreate_RecordsParentSession(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	now := time.Now()
	if err := store.Put(&domain.Session{Name: "orchestrator", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "probe", scope: "session", setup: `jq -nc --arg parent "{{.ParentSession}}" '{parent:$parent}'`}},
		[]nodeFixture{{id: "probe"}})
	writeSetupWorkflow(t, cfg, "wf", providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, workdir, workdir)))

	if _, err := Create(cfg, store, CreateParams{
		URL:           "https://github.com/org/repo/issues/6",
		ParentSession: "orchestrator",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	child := store.Get("org/repo-6+wf")
	if child == nil {
		t.Fatal("child session not persisted")
	}
	if child.ParentSession != "orchestrator" {
		t.Fatalf("ParentSession = %q, want orchestrator", child.ParentSession)
	}
	if got := child.Nodes["probe"].Outputs["parent"]; got != "orchestrator" {
		t.Fatalf("rendered ParentSession = %v, want orchestrator", got)
	}
	parent := store.Get("orchestrator")
	if parent == nil {
		t.Fatal("parent session missing")
	}
	if got, want := parent.Children, []string{"org/repo-6+wf"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parent.Children = %v, want %v", got, want)
	}
}

// The initial_task dispatcher is a session node whose setup shells out to a
// nested `plect task setup … --name initial` that writes its instance
// straight to the store. Create's session-tasks persist must overlay
// (mergeTasks), not blind-Put, or that nested write is clobbered. This
// stands in for the real dispatcher with the nested-write test helper
// (nestedwrite_helper_test.go) so the merge is exercised through the real
// store API without a plect binary.
func TestCreate_SessionNodeNestedWriteSurvives(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	t.Setenv("PLECT_SERVICE_NESTED_WRITE_HELPER", "1")

	// dispatcher writes a sibling "initial" key straight to the store
	// (mimicking the nested `plect task setup`), then produces normally.
	dispatcher := nestedWriteCommand(t, store.Dir(), "org/repo-11+claude", nestedWritePatch{
		Tasks: map[string]*contract.TaskState{
			"initial": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced, TaskID: "work", Name: "initial", Outputs: map[string]any{"instruction": "start work"}},
		},
	}) + `
echo '{}'`
	cfg := writeWorkflowFixture(t, t.TempDir(), "claude",
		[]taskFixture{{id: "dispatcher", scope: "session", setup: dispatcher}},
		[]nodeFixture{{id: "dispatcher"}})
	writeSetupWorkflow(t, cfg, "claude", providerRunningScript("claude", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, workdir, workdir)))

	url := "https://github.com/org/repo/issues/11"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	session := store.Get("org/repo-11+claude")
	if session == nil {
		t.Fatal("session not persisted")
	}
	// The dispatcher node itself produced…
	if st := session.Nodes["dispatcher"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("dispatcher node = %+v, want produced", st)
	}
	// …and its nested write survived the session-tasks persist.
	got := session.Tasks["initial"]
	if got == nil {
		t.Fatal("nested-written 'initial' instance was clobbered by the create persist")
	}
	if got.Name != "initial" || got.TaskID != "work" || got.Outputs["instruction"] != "start work" {
		t.Fatalf("initial instance mismatch: %+v", got)
	}
}

// Binding-implies-delivery applies at session creation too, not only to a
// dynamic task setup: the session's own resource is wired through the same
// subscribeIfWired call TaskSetup makes for an explicit --resource.
func TestCreate_ResourceSubscribesToMatchingProvider(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	subRec := filepath.Join(t.TempDir(), "sub-rec")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "probe", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "probe"}})
	extra := providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, workdir, workdir)) + `
[wf.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%s\n%s\n" "$1" "$2" > "$3"', "provider",
  { from = "session.name" }, { from = "resource.id" }, "` + subRec + `"]
`
	writeSetupWorkflow(t, cfg, "wf", extra)

	url := "https://github.com/org/repo/issues/9"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, readErr := os.ReadFile(subRec)
	if readErr != nil {
		t.Fatalf("subscribe hook did not run: %v", readErr)
	}
	want := "org/repo-9+wf\n" + url + "\n"
	if string(got) != want {
		t.Errorf("subscribe hook recorded %q, want %q", got, want)
	}
}

// An issue session's own subscribe wiring at create time must carry the
// branch its workspace provider's setup hook produced, not just its name and
// resource — this is what lets the GitHub workspace provider's subscribe
// hook forward a session branch to the shipped watcher's linked-PR
// discovery for an issue resource, without a `plect subscribe` action ever
// running.
func TestCreate_ResourceSubscribeCarriesSetupProducedBranch(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	subRec := filepath.Join(t.TempDir(), "sub-rec")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "probe", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "probe"}})
	extra := providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s","branch":"issue/9"}'
`, workdir, workdir)) + `
[wf.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%s" "$1" > "$2"', "provider",
  { from = "session.branch" }, "` + subRec + `"]
`
	writeSetupWorkflow(t, cfg, "wf", extra)

	url := "https://github.com/org/repo/issues/9"
	result, err := Create(cfg, store, CreateParams{URL: url})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if result.Branch != "issue/9" {
		t.Fatalf("precondition: Branch = %q, want issue/9", result.Branch)
	}

	got, readErr := os.ReadFile(subRec)
	if readErr != nil {
		t.Fatalf("subscribe hook did not run: %v", readErr)
	}
	if string(got) != "issue/9" {
		t.Errorf("subscribe hook saw session.branch = %q, want issue/9", got)
	}
}

// A resource no workspace provider hooks for must leave Create unaffected:
// no subscribe hook to run means nothing queued for retry either.
func TestCreate_NoDeliverableResourceBehavesAsBefore(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "probe", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "probe"}})
	writeSetupWorkflow(t, cfg, "wf", providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, workdir, workdir)))

	url := "https://github.com/org/repo/issues/10"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if s := store.Get("org/repo-10+wf"); s == nil {
		t.Fatal("session not persisted")
	}
	if _, err := os.Stat(pendingDeliveryPath(store)); !os.IsNotExist(err) {
		t.Fatalf("subscription retry sidecar should not exist without a failed hook: stat err=%v", err)
	}
}

// The workflow-setup create path must record lifecycle.created for a new
// session (the legacy path did, this one didn't), and a recovery/re-run
// must not append a duplicate.
func TestCreate_WorkflowSetupRecordsLifecycleCreated(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "wf", providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, workdir, workdir)))

	url := "https://github.com/org/repo/issues/7"
	countCreated := func() int {
		evs, _, _, err := EventList(cfg, store, url, 0, event.Filter{Types: []string{"lifecycle.created"}})
		if err != nil {
			t.Fatalf("event list: %v", err)
		}
		return len(evs)
	}

	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n := countCreated(); n != 1 {
		t.Fatalf("lifecycle.created after create = %d, want 1", n)
	}

	// Re-run on the now-existing session must not append a second created.
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if n := countCreated(); n != 1 {
		t.Fatalf("lifecycle.created after re-run = %d, want 1 (no duplicate)", n)
	}
}

func TestCreate_WorkflowSetupFailureLeavesRetryableState(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	gate := filepath.Join(t.TempDir(), "gate")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	// Fails until the gate file exists — simulates a transient gh outage.
	writeSetupWorkflow(t, cfg, "wf", providerRunningScript("wf", fmt.Sprintf(`[ -f %s ] || { echo "transient" >&2; exit 1; }
mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, gate, workdir, workdir)))

	url := "https://github.com/org/repo/issues/6"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err == nil {
		t.Fatal("expected first create to fail")
	}
	s := store.Get("org/repo-6+wf")
	if s == nil {
		t.Fatal("failed setup must still leave an inspectable state entry")
	}
	if st := s.Nodes[contract.WorkflowPseudoNodeID]; st == nil || st.Status != contract.TaskStatusFailed {
		t.Fatalf("pseudo-node = %+v, want failed", st)
	}

	if err := os.WriteFile(gate, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	s = store.Get("org/repo-6+wf")
	if st := s.Nodes[contract.WorkflowPseudoNodeID]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("pseudo-node after retry = %+v, want produced", st)
	}

	// The first attempt left an inspectable state entry but failed before
	// recording lifecycle.created; the retry is the first *success*, so created
	// must be recorded exactly once (not skipped because the entry pre-existed).
	evs, _, _, err := EventList(cfg, store, url, 0, event.Filter{Types: []string{"lifecycle.created"}})
	if err != nil {
		t.Fatalf("event list: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("lifecycle.created after retry-success = %d, want 1", len(evs))
	}
}

// A session torn down must drop its own resource's event-delivery
// registration, through the same locked, durable-retry unsubscribe path
// TaskCleanup runs for a dynamic instance's own bound resource.
func TestDestroy_UnsubscribesSessionResource(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	subRec := filepath.Join(t.TempDir(), "sub-rec")
	unsubRec := filepath.Join(t.TempDir(), "unsub-rec")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "probe", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "probe"}})
	extra := providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, workdir, workdir)) + `
[wf.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%s\n%s\n" "$1" "$2" > "$3"', "provider",
  { from = "session.name" }, { from = "resource.id" }, "` + subRec + `"]

[wf.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf "%s\n%s\n" "$1" "$2" > "$3"', "provider",
  { from = "session.name" }, { from = "resource.id" }, "` + unsubRec + `"]
`
	writeSetupWorkflow(t, cfg, "wf", extra)

	url := "https://github.com/org/repo/issues/12"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.ReadFile(subRec); err != nil {
		t.Fatalf("precondition: subscribe hook did not run at create: %v", err)
	}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "org/repo-12+wf"}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, readErr := os.ReadFile(unsubRec)
	if readErr != nil {
		t.Fatalf("unsubscribe hook did not run: %v", readErr)
	}
	want := "org/repo-12+wf\n" + url + "\n"
	if string(got) != want {
		t.Errorf("unsubscribe hook recorded %q, want %q", got, want)
	}
}

// Destroy's own unsubscribe hook failing must not fail Destroy itself, and
// the durably-queued retry must still drain via an unrelated session's
// later activity, since nothing of the now-deleted session's own can ever
// retry it again.
func TestDestroy_UnsubscribeFailureIsDurablyQueuedAndDrainsViaAnotherSession(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	toggle := filepath.Join(t.TempDir(), "toggle")
	rec := filepath.Join(t.TempDir(), "rec")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{
			{id: "probe", scope: "session", setup: `echo '{}'`},
			{id: "work", scope: "session", setup: `echo '{}'`},
		},
		[]nodeFixture{{id: "probe"}})
	extra := providerRunningScript("wf", fmt.Sprintf(`mkdir -p %s
echo '{"workspace_dir":"%s"}'
`, workdir, workdir)) + `
[wf.subscribe]
type    = "exec"
command = "true"

[wf.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'test -e "$1" || exit 3; echo done > "$2"', "provider", "` + toggle + `", "` + rec + `"]
`
	writeSetupWorkflow(t, cfg, "wf", extra)

	// toggle is absent, so the unsubscribe hook fails.
	url := "https://github.com/org/repo/issues/13"
	deadSession := "org/repo-13+wf"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := Destroy(cfg, store, DestroyParams{Identifier: deadSession}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if store.Get(deadSession) != nil {
		t.Fatal("state entry should be deleted despite the unsubscribe failure")
	}
	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Unsubscribe[deadSession]) != 1 || f.Unsubscribe[deadSession][0] != url {
		t.Fatalf("pending unsubscribe queue after Destroy = %v (err=%v), want [%s] for %s", f.Unsubscribe, loadErr, url, deadSession)
	}

	// deadSession has no state entry left to retry itself, so only an
	// unrelated session's activity can drain it.
	if err := os.WriteFile(toggle, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(cfg, store, CreateParams{URL: "https://github.com/org/repo/issues/14"}); err != nil {
		t.Fatalf("Create (unrelated session): %v", err)
	}
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "org/repo-14+wf"}); err != nil {
		t.Fatalf("TaskSetup (unrelated session): %v", err)
	}

	if _, err := os.Stat(rec); err != nil {
		t.Errorf("deadSession's orphaned unsubscribe hook did not run via the unrelated session's activity: %v", err)
	}
	f, loadErr = loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Unsubscribe[deadSession]) != 0 {
		t.Errorf("pending unsubscribe queue for %s = %v (err=%v), want drained", deadSession, f.Unsubscribe, loadErr)
	}
}

func TestDestroy_RunsWorkflowCleanup(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	marker := filepath.Join(t.TempDir(), "released")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "wf",
		providerCreatingWorkspace("wf", workdir)+fmt.Sprintf(`
[wf.cleanup]
type    = "exec"
command = "sh"
args    = ["-c", 'rm -rf "$1" && touch "$2"', "provider", { from = "self.outputs.workspace_dir" }, %q]

[wf.outputs_schema]
type = "object"

[wf.outputs_schema.properties]
workspace_dir = { type = "string" }
`, marker))

	url := "https://github.com/org/repo/issues/7"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := Destroy(cfg, store, DestroyParams{Identifier: "org/repo-7+wf"})
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("workflow cleanup did not run")
	}
	if !result.RemovedWorkspaceDir {
		t.Error("workdir was removed by cleanup; result should report it")
	}
	if store.Get("org/repo-7+wf") != nil {
		t.Error("state entry should be deleted")
	}
}

// TestDestroy_ForwardsCleanupInputsToWorkflowCleanup pins the generic
// escape hatch destroy operators use to pass provider-specific cleanup
// intents (e.g. --input delete_branch=true) through to provider cleanup,
// without core interpreting them.
func TestDestroy_ForwardsCleanupInputsToWorkflowCleanup(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	marker := filepath.Join(t.TempDir(), "delete_branch.txt")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "wf",
		providerCreatingWorkspace("wf", workdir)+fmt.Sprintf(`
[wf.cleanup]
type    = "exec"
command = "sh"
args = [
  "-c",
  'rm -rf "$1" && printf "%%s" "$2" > "$3"',
  "provider",
  { from = "self.outputs.workspace_dir" },
  { from = "cleanup.inputs.delete_branch", default = "" },
  %q,
]

[wf.outputs_schema]
type = "object"

[wf.outputs_schema.properties]
workspace_dir = { type = "string" }
`, marker))

	url := "https://github.com/org/repo/issues/8"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := Destroy(cfg, store, DestroyParams{
		Identifier:    "org/repo-8+wf",
		CleanupInputs: map[string]string{"delete_branch": "true"},
	}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "true" {
		t.Errorf("cleanup saw .CleanupInputs.delete_branch = %q, want true", got)
	}
}

func TestDestroy_WorkflowCleanupFailureBlocksWithoutForce(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "wf",
		providerRunningScript("wf", fmt.Sprintf("mkdir -p %s\nprintf '{\"workspace_dir\":\"%s\"}'\n", workdir, workdir))+
			"\n[wf.cleanup]\ntype = \"exec\"\ncommand = \"sh\"\nargs = [\"-c\", \"exit 9\"]\n")

	url := "https://github.com/org/repo/issues/8"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := Destroy(cfg, store, DestroyParams{Identifier: "org/repo-8+wf"})
	if err == nil {
		t.Fatal("expected destroy to fail-fast on cleanup error")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should hint at --force: %v", err)
	}
	if store.Get("org/repo-8+wf") == nil {
		t.Error("state entry must survive a blocked destroy")
	}

	result, err := Destroy(cfg, store, DestroyParams{Identifier: "org/repo-8+wf", Force: true})
	if err != nil {
		t.Fatalf("forced destroy: %v", err)
	}
	if len(result.CleanupWarnings) == 0 {
		t.Error("forced destroy should surface the cleanup failure as a warning")
	}
	if store.Get("org/repo-8+wf") != nil {
		t.Error("forced destroy should delete the state entry")
	}
}

func TestUp_RecoversFailedWorkflowSetup(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	gate := filepath.Join(t.TempDir(), "gate")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "wf", providerRunningScript("wf", fmt.Sprintf(
		"[ -f %s ] || { echo transient >&2; exit 1; }\nmkdir -p %s\nprintf '{\"workspace_dir\":\"%s\"}'\n",
		gate, workdir, workdir)))

	url := "https://github.com/org/repo/issues/9"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err == nil {
		t.Fatal("expected first create to fail")
	}
	if err := os.WriteFile(gate, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	// docker compose up-style: Up with a URL must auto-recover the failed setup.
	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("Up should recover the partial create: %v", err)
	}
	s := store.Get("org/repo-9+wf")
	if st := s.Nodes[contract.WorkflowPseudoNodeID]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("pseudo-node = %+v, want produced after Up recovery", st)
	}
}
