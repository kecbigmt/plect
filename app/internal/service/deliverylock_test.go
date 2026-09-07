package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/flocktest"
	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TestDeliveryLock_ConcurrentSetupWaitsOutACleanupsUnsubscribe pins the
// close (not just narrow) of the race a fresh state read alone cannot
// close: TaskCleanup deciding "not needed" and then TaskSetup binding a new
// instance to the same resource before the unsubscribe hook actually runs.
// The unsubscribe hook is deliberately slow, so a concurrent TaskSetup for
// the same resource is guaranteed to still be waiting on the delivery lock
// when it starts; if TaskSetup's own subscribe call could run at the same
// time, it would race the unsubscribe hook to the workspace provider's
// registry and could lose. With the lock, no such race is possible: the
// concurrent TaskSetup's subscribe always runs strictly after the
// unsubscribe hook completes, so the final registry state is always
// "subscribed" — never coin-flipped.
func TestDeliveryLock_ConcurrentSetupWaitsOutACleanupsUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "unsubscribe-started")
	state := filepath.Join(dir, "registry-state")
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
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

[fixture.health.alive]
type = "noop"

[fixture.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf subscribed > "$1"', "provider", "` + state + `"]

[fixture.unsubscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'touch "$1"; sleep 0.3; printf unsubscribed > "$2"', "provider", "` + started + `", "` + state + `"]
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
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "a", Resource: prURL}); err != nil {
		t.Fatalf("setup a: %v", err)
	}

	cleanupDone := make(chan error, 1)
	go func() {
		_, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "a", SessionName: "sess-1"})
		cleanupDone <- err
	}()

	// Wait until TaskCleanup is inside its unsubscribe hook (holding the
	// delivery lock and about to sleep) before starting the concurrent
	// TaskSetup — otherwise a b that just happens to run after cleanup
	// finishes entirely would prove nothing about the lock.
	waitUntil(t, func() bool { _, err := os.Stat(started); return err == nil })

	setupStart := time.Now()
	setupResult, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "b", Resource: prURL})
	setupElapsed := time.Since(setupStart)
	if err != nil {
		t.Fatalf("setup b: %v", err)
	}
	if !setupResult.Subscribed {
		t.Fatalf("setup b did not subscribe (SubscribeError=%q)", setupResult.SubscribeError)
	}
	// The unsubscribe hook sleeps 300ms; if b's own subscribe call had to
	// wait out the delivery lock, this call cannot have returned in much
	// less than that.
	if setupElapsed < 250*time.Millisecond {
		t.Errorf("setup b returned in %s, want it to have waited out the delivery lock (~300ms)", setupElapsed)
	}

	if err := <-cleanupDone; err != nil {
		t.Fatalf("cleanup a: %v", err)
	}

	got, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "subscribed" {
		t.Errorf("final registry state = %q, want %q — b's subscribe must always run after a's unsubscribe completes, never race it", got, "subscribed")
	}
}

// A failure to acquire the delivery lock itself — not just a failure inside
// it — must still be queued for a durable retry: subscribeIfWired never
// even runs, so the only trace of the failure without a queue entry would
// be the transient SubscribeError.
func TestTaskSetup_DeliveryLockAcquisitionFailureQueuesSubscribeRetry(t *testing.T) {
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

[fixture.health.alive]
type = "noop"

[fixture.subscribe]
type    = "exec"
command = "sh"
args    = ["-c", 'printf done > "$1"', "provider", "` + rec + `"]
`
	if err := os.MkdirAll(filepath.Join(cfg.BaseDir, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, "workspaces", "fixture.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	seedSession(t, store, "sess-1", "sess", 1, "coding", map[string]*contract.TaskState{})

	// A regular file where withDeliveryLock needs to create a directory
	// makes its os.MkdirAll fail before subscribeIfWired ever runs.
	if err := os.WriteFile(filepath.Join(store.Dir(), "delivery-locks"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	const prURL = "resource://sess/proj/pull/9"
	result, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "pr", Resource: prURL})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if result.Subscribed {
		t.Error("Subscribed = true, want false: the delivery lock could not be acquired")
	}
	if result.SubscribeError == "" {
		t.Error("SubscribeError is empty, want the lock-acquisition failure reported")
	}
	if _, statErr := os.Stat(rec); statErr == nil {
		t.Error("the subscribe hook must not have run: the lock was never acquired")
	}

	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := f.Subscribe["sess-1"]; len(got) != 1 || got[0] != prURL {
		t.Fatalf("pending subscribe queue = %v, want [%s] for sess-1", got, prURL)
	}
}

// The unsubscribe-side counterpart: a failure to acquire the delivery lock
// during TaskCleanup's own decision must also be queued, not just reported
// transiently — the reclaimed instance record is already gone, so the
// queue is the only surviving trace.
func TestTaskCleanup_DeliveryLockAcquisitionFailureQueuesUnsubscribeRetry(t *testing.T) {
	cfg := writeWorkflowFixture(t, t.TempDir(), "coding",
		[]taskFixture{{id: "work", scope: "session", setup: `echo '{}'`, cleanup: "true"}},
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

[fixture.health.alive]
type = "noop"

[fixture.subscribe]
type    = "exec"
command = "true"

[fixture.unsubscribe]
type    = "exec"
command = "true"
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
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: "sess-1", Name: "a", Resource: prURL}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Sabotage the lock directory after setup succeeds (setup's own
	// subscribe already created it as a real directory, so it must be
	// cleared first), so TaskCleanup's own unsubscribe decision is what
	// fails to acquire it.
	lockDir := filepath.Join(store.Dir(), "delivery-locks")
	if err := os.RemoveAll(lockDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := TaskCleanup(cfg, store, TaskCleanupParams{Instance: "a", SessionName: "sess-1"})
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if result.Unsubscribed {
		t.Error("Unsubscribed = true, want false: the delivery lock could not be acquired")
	}
	if result.UnsubscribeError == "" {
		t.Error("UnsubscribeError is empty, want the lock-acquisition failure reported")
	}

	f, loadErr := loadPendingDelivery(pendingDeliveryPath(store))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := f.Unsubscribe["sess-1"]; len(got) != 1 || got[0] != prURL {
		t.Fatalf("pending unsubscribe queue = %v, want [%s] for sess-1", got, prURL)
	}
}

// The Linux NFS client rejects LOCK_EX on an O_RDONLY descriptor with EBADF,
// even though local filesystems tolerate it. This test inspects the lock
// file descriptor's own open flags via /proc, so it catches the regression
// even on a local (non-NFS) test filesystem.
func TestWithDeliveryLock_OpensLockFileWritable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("lock fd flags are inspected via /proc, which is Linux-specific")
	}

	store := state.NewStore(t.TempDir())
	lockPath := filepath.Join(store.Dir(), "delivery-locks", "sess-1.lock")

	var accErr error
	err := withDeliveryLock(store, "sess-1", func() {
		accMode, err := flocktest.AccessMode(lockPath)
		if err != nil {
			accErr = err
			return
		}
		if accMode == os.O_RDONLY {
			accErr = fmt.Errorf("lock file opened O_RDONLY; exclusive lock (LOCK_EX) requires a writable descriptor on NFS")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if accErr != nil {
		t.Error(accErr)
	}
}
