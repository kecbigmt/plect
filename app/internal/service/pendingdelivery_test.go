package service

import (
	"os"
	"path/filepath"
	"testing"

	contract "github.com/kecbigmt/plecture/contracts/state"
)

// A TaskCleanup whose unsubscribe hook fails queues the resource durably —
// the instance record that would otherwise be the retry handle is already
// gone by the time this runs, so the queue is the only surviving trace of
// the failure.
func TestTaskCleanup_UnsubscribeFailureIsDurablyQueued(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	writeAlwaysFailingUnsubscribeProvider(t, cfg.BaseDir)
	store := testStore(t)
	seedSession(t, store, "sess-1", "sess", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "resource://sess/proj/pull/9"
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "pr", Resource: prURL}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	result, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "pr", SessionName: "sess-1"})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.Unsubscribed || result.UnsubscribeError == "" {
		t.Fatalf("result = %+v, want a failed-but-non-fatal unsubscribe", result)
	}

	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := f.Unsubscribe["sess-1"]; len(got) != 1 || got[0] != prURL {
		t.Fatalf("pending unsubscribe queue = %v, want [%s] for sess-1", got, prURL)
	}
}

// A TaskSetup whose subscribe hook fails queues the resource for a durable
// retry too — the symmetric case of the unsubscribe side above.
func TestTaskSetup_SubscribeFailureIsDurablyQueued(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "work"}},
	)
	body := `
[fixture]
kind  = "workspace_provider"
match = '` + fixtureResourceMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[fixture.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[fixture.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", "exit 3"]
`
	if err := os.MkdirAll(filepath.Join(cfg.BaseDir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "workspaces", "fixture.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	seedSession(t, store, "sess-1", "sess", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "resource://sess/proj/pull/9"
	result, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "pr", Resource: prURL})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if result.Subscribed || result.SubscribeError == "" {
		t.Fatalf("result = %+v, want a failed-but-non-fatal subscribe", result)
	}

	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := f.Subscribe["sess-1"]; len(got) != 1 || got[0] != prURL {
		t.Fatalf("pending subscribe queue = %v, want [%s] for sess-1", got, prURL)
	}
}

// writeAlwaysFailingUnsubscribeProvider drops a provider whose subscribe
// hook succeeds and whose unsubscribe hook always fails.
func writeAlwaysFailingUnsubscribeProvider(t *testing.T, baseDir string) {
	t.Helper()
	body := `
[fixture]
kind  = "workspace_provider"
match = '` + fixtureResourceMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[fixture.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[fixture.subscribe]
type    = "exec"
command = "true"

[fixture.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", "exit 3"]
`
	if err := os.MkdirAll(filepath.Join(baseDir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "workspaces", "fixture.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// toggledUnsubscribeProvider's unsubscribe hook fails until toggle exists,
// then succeeds and records its call to rec — so a test can flip a real
// hook from "always fails" to "works" between a queuing attempt and a
// later retry.
func toggledUnsubscribeProvider(t *testing.T, baseDir, toggle, rec string) {
	t.Helper()
	body := `
[fixture]
kind  = "workspace_provider"
match = '` + fixtureResourceMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[fixture.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[fixture.subscribe]
type    = "exec"
command = "true"

[fixture.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'test -e "$1" || exit 3; echo done > "$2"', "provider", "` + toggle + `", "` + rec + `"]
`
	if err := os.MkdirAll(filepath.Join(baseDir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "workspaces", "fixture.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The queue drains on the session's next activity, once the hook starts
// working — this is the durable-retry path TaskCleanup's own failure alone
// cannot provide, since its instance record (the natural retry handle) is
// already gone by the time the hook fails.
func TestFlushPendingDelivery_RetriesUnsubscribeAndDrainsOnSuccess(t *testing.T) {
	toggle := filepath.Join(t.TempDir(), "toggle")
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	toggledUnsubscribeProvider(t, cfg.BaseDir, toggle, rec)
	store := testStore(t)
	seedSession(t, store, "sess-1", "sess", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "resource://sess/proj/pull/9"
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "pr", Resource: prURL}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "pr", SessionName: "sess-1"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Unsubscribe["sess-1"]) != 1 {
		t.Fatalf("precondition: expected %s queued, got %v (err=%v)", prURL, f.Unsubscribe, loadErr)
	}

	if err := os.WriteFile(toggle, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if errs := flushPendingDelivery(cfg, store, "sess-1"); len(errs) != 0 {
		t.Fatalf("flushPendingDelivery: %v", errs)
	}

	if _, err := os.Stat(rec); err != nil {
		t.Errorf("the retried unsubscribe hook did not run: %v", err)
	}
	f, loadErr = loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Unsubscribe["sess-1"]) != 0 {
		t.Errorf("pending unsubscribe queue = %v (err=%v), want empty after a successful retry", f.Unsubscribe, loadErr)
	}
}

// A resource queued under a session with no state entry (the post-destroy
// case) must still drain, through a wholly unrelated session's activity.
func TestFlushPendingDeliveryLogged_DrainsOrphanedEntryViaAnotherSessionsActivity(t *testing.T) {
	toggle := filepath.Join(t.TempDir(), "toggle")
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "work"}},
	)
	toggledUnsubscribeProvider(t, cfg.BaseDir, toggle, rec)
	store := testStore(t)

	const prURL = "resource://sess/proj/pull/9"
	seedSession(t, store, "gone-1", "gone", 1, "coding", map[string]*contract.TaskState{})
	gone := store.Get("gone-1")
	if gone == nil {
		t.Fatal("missing queued session")
	}
	if err := store.Destroy("gone-1"); err != nil {
		t.Fatal(err)
	}
	if err := queuePendingUnsubscribeForSessionID(store, gone.ID, prURL); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(toggle, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}

	seedSession(t, store, "other-1", "other", 1, "coding", map[string]*contract.TaskState{})
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "other-1"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if _, err := os.Stat(rec); err != nil {
		t.Errorf("gone-1's orphaned unsubscribe hook did not run via other-1's activity: %v", err)
	}
	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Unsubscribe["gone-1"]) != 0 {
		t.Errorf("pending unsubscribe queue for gone-1 = %v (err=%v), want drained", f.Unsubscribe, loadErr)
	}
}

// A resource re-bound by another instance since the original failure is
// dropped from the queue without running the hook: whatever queued it is
// moot once something else has since claimed the resource again.
func TestFlushPendingDelivery_DropsUnsubscribeEntryOnceResourceIsNeededAgain(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	// A toggle that never appears: if the hook ran at all, the test fails.
	toggledUnsubscribeProvider(t, cfg.BaseDir, filepath.Join(t.TempDir(), "never"), rec)
	store := testStore(t)
	seedSession(t, store, "sess-1", "sess", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "resource://sess/proj/pull/9"
	// Bind the resource to a live instance BEFORE queuing: TaskSetup flushes
	// the queue itself at its own start, so queuing first would let that
	// internal flush (running before "again" exists) drop the entry for the
	// wrong reason.
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "again", Resource: prURL}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := queuePendingUnsubscribe(store, "sess-1", prURL); err != nil {
		t.Fatal(err)
	}

	if errs := flushPendingDelivery(cfg, store, "sess-1"); len(errs) != 0 {
		t.Fatalf("flushPendingDelivery: %v", errs)
	}

	if _, err := os.Stat(rec); err == nil {
		t.Error("a resource needed again must not have its unsubscribe hook run")
	}
	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Unsubscribe["sess-1"]) != 0 {
		t.Errorf("pending unsubscribe queue = %v (err=%v), want the now-needed entry dropped", f.Unsubscribe, loadErr)
	}
}

// A queued subscribe drains once the hook starts working, symmetric to the
// unsubscribe-side retry test above.
func TestFlushPendingDelivery_RetriesSubscribeAndDrainsOnSuccess(t *testing.T) {
	toggle := filepath.Join(t.TempDir(), "toggle")
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`}},
		[]nodeFixture{{id: "work"}},
	)
	body := `
[fixture]
kind  = "workspace_provider"
match = '` + fixtureResourceMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[fixture.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[fixture.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'test -e "$1" || exit 3; echo done > "$2"', "provider", "` + toggle + `", "` + rec + `"]
`
	if err := os.MkdirAll(filepath.Join(cfg.BaseDir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "workspaces", "fixture.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	seedSession(t, store, "sess-1", "sess", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "resource://sess/proj/pull/9"
	setupResult, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "pr", Resource: prURL})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if setupResult.Subscribed {
		t.Fatalf("precondition: expected the subscribe hook to fail, got Subscribed=true")
	}

	if err := os.WriteFile(toggle, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if errs := flushPendingDelivery(cfg, store, "sess-1"); len(errs) != 0 {
		t.Fatalf("flushPendingDelivery: %v", errs)
	}

	if _, err := os.Stat(rec); err != nil {
		t.Errorf("the retried subscribe hook did not run: %v", err)
	}
	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Subscribe["sess-1"]) != 0 {
		t.Errorf("pending subscribe queue = %v (err=%v), want empty after a successful retry", f.Subscribe, loadErr)
	}
}

// A queued subscribe for a resource no instance needs any more (its only
// binding was itself reclaimed) is dropped without retrying.
func TestFlushPendingDelivery_DropsSubscribeEntryOnceResourceIsNoLongerNeeded(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "rec")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
		[]nodeFixture{{id: "work"}},
	)
	// A toggle that never appears: if the hook ran at all, the test fails.
	body := `
[fixture]
kind  = "workspace_provider"
match = '` + fixtureResourceMatch + `'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[fixture.setup]
type    = "exec"
command = "printf"
args    = ['{"workdir":"/tmp/x"}']

[fixture.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'test -e "$1" || exit 3; echo done > "$2"', "provider", "` + filepath.Join(t.TempDir(), "never") + `", "` + rec + `"]
`
	if err := os.MkdirAll(filepath.Join(cfg.BaseDir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "workspaces", "fixture.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	seedSession(t, store, "sess-1", "sess", 1, "coding", map[string]*contract.TaskState{})

	const prURL = "resource://sess/proj/pull/9"
	if err := queuePendingSubscribe(store, "sess-1", prURL); err != nil {
		t.Fatal(err)
	}

	if errs := flushPendingDelivery(cfg, store, "sess-1"); len(errs) != 0 {
		t.Fatalf("flushPendingDelivery: %v", errs)
	}

	if _, err := os.Stat(rec); err == nil {
		t.Error("a resource nothing needs must not have its subscribe hook run")
	}
	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil || len(f.Subscribe["sess-1"]) != 0 {
		t.Errorf("pending subscribe queue = %v (err=%v), want the now-moot entry dropped", f.Subscribe, loadErr)
	}
}

// flushPendingDeliveryLogged must not silently discard flushPendingDelivery's
// own errors: TaskSetup/TaskCleanup have no result field for "an unrelated
// queued resource's retry also failed just now," so the log is the only
// place this becomes visible.
