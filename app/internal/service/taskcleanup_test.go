package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/plugins"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// cleanup runs the instance's cleanup script and removes it from state so the
// name is free for a subsequent setup (the dispatcher's recreate idiom).
func TestTaskCleanup_RemovesInstanceAndRunsScript(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "touch " + marker}},
		[]nodeFixture{{id: "work"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "initial"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"})
	if err != nil {
		t.Fatalf("TaskCleanup: %v", err)
	}
	if !res.Found {
		t.Error("Found = false, want true for a present instance")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("cleanup script did not run: %v", err)
	}
	if store.Get("o/r-1").Tasks["initial"] != nil {
		t.Error("instance must be removed from state (frees the name for re-setup)")
	}
}

// Removing the instance lets a same-name setup recreate without colliding —
// this is exactly the dispatcher's `cleanup initial; setup … --name initial`.
func TestTaskCleanup_FreesNameForReSetup(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "initial"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "initial"}); err != nil {
		t.Fatalf("re-setup after cleanup must not collide: %v", err)
	}
}

// An absent instance is a no-op success (Found=false), so the dispatcher's
// first-run `cleanup initial` (nothing yet) is harmless.
func TestTaskCleanup_AbsentIsNoOp(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "work"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	res, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"})
	if err != nil {
		t.Fatalf("cleanup of absent instance must be a no-op, got: %v", err)
	}
	if res.Found {
		t.Error("Found = true, want false for an absent instance")
	}
}

// A concurrent TaskSetup that reserves a new instance during a slow cleanup
// must survive: the cleanup persists only its own key, not a stale whole-session
// snapshot that would clobber the new reservation.
func TestTaskCleanup_DoesNotClobberConcurrentSetup(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	gate := filepath.Join(dir, "go")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "slow", scope: "session", setup: `echo '{}'`,
				cleanup: `touch ` + started + `; while [ ! -e ` + gate + ` ]; do sleep 0.01; done`},
			{id: "fast", scope: "session", setup: `echo '{}'`},
		},
		[]nodeFixture{{id: "slow"}, {id: "fast"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "slow", SessionName: "o/r-1", Name: "victim"}); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "victim", SessionName: "o/r-1"})
		done <- err
	}()

	// Once the cleanup script is running, reserve a fresh instance concurrently.
	waitUntil(t, func() bool { _, err := os.Stat(started); return err == nil })
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "fast", SessionName: "o/r-1", Name: "fresh"}); err != nil {
		t.Fatalf("concurrent setup: %v", err)
	}

	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	s := store.Get("o/r-1")
	if s.Tasks["victim"] != nil {
		t.Error("victim should be reclaimed")
	}
	if s.Tasks["fresh"] == nil {
		t.Error("concurrently-reserved 'fresh' was clobbered by the cleanup persist")
	}
}

// cleanup reclaims by key regardless of which task produced it — a task
// drift that left `initial` on the wrong task is still swept.
func TestTaskCleanup_ReclaimsRegardlessOfTaskID(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"},
			{id: "review", scope: "session", setup: `echo '{}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "work"}, {id: "review"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	// `initial` was produced by the wrong task (review).
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "review", SessionName: "o/r-1", Name: "initial"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	res, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"})
	if err != nil || !res.Found {
		t.Fatalf("cleanup initial = (%+v, %v), want Found", res, err)
	}
	if store.Get("o/r-1").Tasks["initial"] != nil {
		t.Error("initial should be reclaimed even though it was a review instance")
	}
}

// When the cleanup script fails, TaskCleanup persists the failed status so
// it's inspectable for retry. If that persist itself fails, the error must
// not be discarded — it's the only sign the failed status never landed.
// The cleanup script corrupts state.json itself before exiting non-zero, so
// the persist attempted right after the (already-successful) session lookup
// is what fails.
func TestTaskCleanup_JoinsPersistFailureWithCleanupFailure(t *testing.T) {
	store := testStore(t)
	statePath := filepath.Join(store.Dir(), "state.json")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`,
			cleanup: "printf '{not valid json' > " + statePath + " && exit 1"}},
		[]nodeFixture{{id: "work"}},
	)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "initial"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"})
	if err == nil {
		t.Fatal("expected an error: the cleanup script fails and the post-failure persist also fails")
	}
	if !strings.Contains(err.Error(), "state:") {
		t.Errorf("expected the persist failure to be joined into the returned error, got: %s", err.Error())
	}
}

// A partial unwind's record has to survive the failure that produced it: the
// per-layer outcome is what tells the retry which layers are still owed, and
// it lives only in the snapshot the cleanup mutated in place. Without it the
// retry reloads every layer as produced and releases one twice.
func TestTaskCleanup_NestedPartialUnwindSurvivesForTheRetry(t *testing.T) {
	dir := t.TempDir()
	innerLog := filepath.Join(dir, "inner-runs")
	gate := filepath.Join(dir, "outer-may-succeed")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "runtime", scope: "session", setup: `echo '{}'`, cleanup: "echo ran >> " + innerLog},
			{id: "team_runtime", scope: "session", cleanup: "test -f " + gate,
				extra: "inner = \"runtime\"\n"},
		},
		[]nodeFixture{{id: "team_runtime"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "team_runtime", SessionName: "o/r-1", Name: "initial"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"}); err == nil {
		t.Fatal("TaskCleanup: want the outer layer's cleanup to fail")
	}

	st := store.Get("o/r-1").Tasks["initial"]
	if st == nil {
		t.Fatal("a failed cleanup must leave the instance inspectable for retry")
	}
	if len(st.Layers) != 2 {
		t.Fatalf("Layers = %+v, want both layer records persisted", st.Layers)
	}
	if st.Layers[1].Status != contract.TaskStatusCleaned {
		t.Errorf("inner layer = %q, want %q — it released successfully", st.Layers[1].Status, contract.TaskStatusCleaned)
	}
	if st.Layers[0].Status != contract.TaskStatusFailed {
		t.Errorf("outer layer = %q, want %q", st.Layers[0].Status, contract.TaskStatusFailed)
	}

	// The operator fixes what made the outer layer fail and retries.
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	runs, err := os.ReadFile(innerLog)
	if err != nil {
		t.Fatalf("read inner cleanup log: %v", err)
	}
	if got := strings.Count(string(runs), "ran"); got != 1 {
		t.Errorf("inner cleanup ran %d times, want exactly once — the retry owes only the layer that failed", got)
	}
	if store.Get("o/r-1").Tasks["initial"] != nil {
		t.Error("a successful retry must reclaim the instance")
	}
}

// TestTaskCleanup_NestedOuterReleasesItsProjectedLocal drives the teardown
// path the way a session's own destroy does: the layer chain is rebuilt from
// the declaration alone, with no compiled plan in scope. An outer layer
// releases what its setup produced as a private local by projecting it into
// its own public contract, so that projection has to survive the rebuild —
// otherwise the release resolves against nothing and the resource leaks.
func TestTaskCleanup_NestedOuterReleasesItsProjectedLocal(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	guard := filepath.Join(dir, "guard")
	released := filepath.Join(dir, "released")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{
			{id: "runtime", scope: "session", setup: `echo '{"pid":"42"}'`},
			{
				id:      "team_runtime",
				scope:   "session",
				setup:   "mkdir -p " + guard + ` && echo '{"guard_dir":"` + guard + `"}'`,
				cleanup: "echo {{.Self.guard_dir}} > " + released,
				extra: "inner = \"runtime\"\n" + `
[outputs.bind]
guard_dir = { from = "locals.guard_dir" }

[locals_schema]
type     = "object"
required = ["guard_dir"]

[locals_schema.properties.guard_dir]
type = "string"

[outputs_schema]
type = "object"

[outputs_schema.properties.guard_dir]
type = "string"
`,
			},
		},
		[]nodeFixture{{id: "team_runtime"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "team_runtime", SessionName: "o/r-1", Name: "initial"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "initial", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("TaskCleanup: %v", err)
	}
	data, err := os.ReadFile(released)
	if err != nil {
		t.Fatalf("the outer layer's cleanup never ran: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != guard {
		t.Errorf("released %q, want the local the outer setup produced (%q)", got, guard)
	}
}

// TaskCleanup drops the delivery registration a matching TaskSetup wired,
// once nothing else in the session still needs it — the counterpart of
// TestTaskSetup_ResourceSubscribesToMatchingProvider.
func TestTaskCleanup_UnsubscribesResourceWhenNoLongerNeeded(t *testing.T) {
	unsubRec := filepath.Join(t.TempDir(), "unsub-rec")
	provCfg := writeSubscribeUnsubscribeProvider(t, "github", ghMatch, "", unsubRec)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	mergeWorkspacesDir(t, provCfg, cfg)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "https://github.com/o/r/pull/9"
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "pr", Resource: prURL}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "pr", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	got, err := os.ReadFile(unsubRec)
	if err != nil {
		t.Fatalf("unsubscribe hook did not run: %v", err)
	}
	if want := "o/r-1\n" + prURL + "\n"; string(got) != want {
		t.Errorf("unsubscribe hook recorded %q, want %q", got, want)
	}
}

// A dynamic instance bound to the session's own primary resource must not
// have delivery unregistered on cleanup: the primary subscription outlives
// any one instance and is not this instance's to drop.
func TestTaskCleanup_DoesNotUnsubscribeSessionPrimaryResource(t *testing.T) {
	unsubRec := filepath.Join(t.TempDir(), "unsub-rec")
	provCfg := writeSubscribeUnsubscribeProvider(t, "github", ghMatch, "", unsubRec)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	mergeWorkspacesDir(t, provCfg, cfg)
	store := testStore(t)
	// seedSession's ResourceID is derived from (ownerRepo, number): this is
	// the session's own primary resource.
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})
	primary := "https://github.com/o/r/issues/1"

	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "again", Resource: primary}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "again", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(unsubRec); err == nil {
		t.Error("cleanup must not unsubscribe the session's own primary resource")
	}
}

// Two instances bound to the same non-primary resource share one delivery
// registration; cleaning up one must leave it wired for the other.
func TestTaskCleanup_DoesNotUnsubscribeResourceStillBoundByAnotherInstance(t *testing.T) {
	unsubRec := filepath.Join(t.TempDir(), "unsub-rec")
	provCfg := writeSubscribeUnsubscribeProvider(t, "github", ghMatch, "", unsubRec)
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	mergeWorkspacesDir(t, provCfg, cfg)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "https://github.com/o/r/pull/9"
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "a", Resource: prURL}); err != nil {
		t.Fatalf("setup a: %v", err)
	}
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "b", Resource: prURL}); err != nil {
		t.Fatalf("setup b: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "a", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("cleanup a: %v", err)
	}
	if _, err := os.Stat(unsubRec); err == nil {
		t.Fatal("cleanup must not unsubscribe while a sibling instance still binds the resource")
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "b", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("cleanup b: %v", err)
	}
	if _, err := os.Stat(unsubRec); err != nil {
		t.Error("cleanup of the last instance bound to the resource must unsubscribe it")
	}
}

// A provider that supports subscribe but not unsubscribe leaves nothing to
// run at cleanup time; Unsubscribed must report that honestly rather than
// inferring "unsubscribed" from "no longer needed" alone.
func TestTaskCleanup_DoesNotFalselyReportUnsubscribedWhenNoHookDeclared(t *testing.T) {
	subRec := filepath.Join(t.TempDir(), "sub-rec")
	provCfg := writeSubscribeUnsubscribeProvider(t, "github", ghMatch, subRec, "")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	mergeWorkspacesDir(t, provCfg, cfg)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "https://github.com/o/r/pull/9"
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "pr", Resource: prURL}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	result, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "pr", SessionName: "o/r-1"})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.Unsubscribed {
		t.Error("Unsubscribed = true, want false: the provider declares no unsubscribe hook to run")
	}
}

// A failing unsubscribe hook must not fail TaskCleanup: the instance and its
// own cleanup script already succeeded by the time this runs, so failing the
// call now couldn't undo that. The failure is durably queued for retry (the
// instance record itself is already gone, so the queue is the only
// surviving retry handle) and reported to this call's own caller via
// UnsubscribeError.
func TestTaskCleanup_UnsubscribeHookFailureDoesNotFailCleanup(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	body := `
[github]
kind  = "workspace_provider"
match = '` + ghMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[github.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[github.subscribe]
type    = "exec"
command = "true"

[github.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", "echo boom >&2; exit 3"]
`
	if err := os.MkdirAll(filepath.Join(cfg.BaseDir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "workspaces", "github.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "https://github.com/o/r/pull/9"
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "pr", Resource: prURL}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	result, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "pr", SessionName: "o/r-1"})
	if err != nil {
		t.Fatalf("TaskCleanup must not fail on a failed unsubscribe hook, got: %v", err)
	}
	if !result.Found {
		t.Error("Found = false, want true: the instance itself was reclaimed")
	}
	if result.Unsubscribed {
		t.Error("Unsubscribed = true, want false: the hook failed")
	}
	if result.UnsubscribeError == "" {
		t.Error("UnsubscribeError is empty, want the hook failure reported")
	}
}

// An instance created without --resource has nothing to unregister; cleanup
// must not fail just because no workspace provider is even configured.
func TestTaskCleanup_NoResourceBoundIsANoOpForDelivery(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	store := testStore(t)
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{})

	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "o/r-1", Name: "x"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "x", SessionName: "o/r-1"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// A session destroyed between resolveSession and TaskCleanup's fresh
// delivery-lock read must not silently skip the unsubscribe decision: a nil
// session means nothing can need the resource any more than an explicit
// "not needed" answer already covers.
func TestShouldUnsubscribe(t *testing.T) {
	tests := []struct {
		name     string
		session  *domain.Session
		resource string
		want     bool
	}{
		{"nil session (destroyed mid-cleanup) has nothing left to need it", nil, "r", true},
		{"present session still needs the resource as its primary", &domain.Session{ResourceID: "r"}, "r", false},
		{"present session no longer needs the resource", &domain.Session{ResourceID: "other"}, "r", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldUnsubscribe(tc.session, tc.resource); got != tc.want {
				t.Errorf("shouldUnsubscribe(%+v, %q) = %v, want %v", tc.session, tc.resource, got, tc.want)
			}
		})
	}
}

// mergeWorkspacesDir copies a fixture's workspaces/ directory (built by
// writeSubscribeUnsubscribeProvider, which allocates its own BaseDir) into
// the target cfg's BaseDir, so a test can compose a workflow fixture with a
// resource-delivery provider fixture without one helper needing to know
// about the other.
func mergeWorkspacesDir(t *testing.T, from, to *config.Config) {
	t.Helper()
	srcDir := filepath.Join(from.BaseDir, "workspaces")
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	dstDir := filepath.Join(to.BaseDir, "workspaces")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A workflow node's instance stores no task id, so cleanup has only the node
// id to find the effect by — and a node id is not the address a plugin's
// effect answers to. Getting this wrong is silent: the definition simply looks
// absent, and cleanup is deliberately tolerant of an absent definition, so the
// script would be skipped rather than reported.
func TestTaskCleanup_NodeInstanceRunsAPluginOwnedEffectsCleanup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleaned")
	pluginDir, base := t.TempDir(), t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pluginDir, "config", "tasks", "runner.toml"), `
[runner]
kind  = "effect"
scope = "session"

[runner.setup]
type   = "shell"
script = "echo '{}'"

[runner.health.alive]
type = "noop"

[runner.cleanup]
type   = "shell"
script = "touch `+marker+`"
`)
	write(filepath.Join(base, "workflows", "coding.toml"), `
[coding]
kind = "workflow"

[[coding.nodes]]
id   = "runner"
uses = "official.acme.runner"
`)
	cfg := &config.Config{
		BaseDir:    base,
		PluginDirs: []string{pluginDir},
		Plugins:    []plugins.Mounted{{ID: "official/acme", Dir: pluginDir}},
	}
	store := testStore(t)
	// TaskID omitted, as it is for a node whose id equals the definition's.
	seedSession(t, store, "o/r-1", "o/r", 1, "coding", map[string]*contract.TaskState{
		"runner": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
	})

	res, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "runner", SessionName: "o/r-1"})
	if err != nil {
		t.Fatalf("TaskCleanup: %v", err)
	}
	if !res.Found {
		t.Fatal("Found = false, want true")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the plugin effect's cleanup script did not run: %v", err)
	}
}
