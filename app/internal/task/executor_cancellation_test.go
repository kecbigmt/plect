package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/effect"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// These tests cover cancellation for the five task exec paths (setup,
// cleanup, alive probe, capture, dynamic-output fetch): each accepts a
// context.Context, and cancelling it must terminate the child process (so
// the marker file it would otherwise write never appears) and the error
// must surface to the caller promptly instead of the call blocking for the
// child's full lifetime.

const cancellationCharChildSleep = 8 * time.Second

// Must exceed effect.CancelWaitDelay: the executor's own fallback can take
// that long, so a tighter budget fails even when it works as designed.
const cancellationCharKillBudget = effect.CancelWaitDelay + 2*time.Second

const cancellationCharFastKillBudget = effect.CancelWaitDelay / 2

// assertGroupKillFast fails the test if a hung child in the same process
// group as the executor took long enough to return that the WaitDelay
// fallback, not the group kill, must have been what actually ended it — a
// regression the wider cancellationCharKillBudget alone can't catch, since
// it also passes a run that fell all the way through to WaitDelay. Gated to
// linux, where the bound is deterministic; darwin's scheduler has made it
// flaky in CI, so darwin relies on cancellationCharKillBudget alone (the
// escaped-grandchild test covers that fallback path deliberately).
func assertGroupKillFast(t *testing.T, elapsed time.Duration, opName string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	if elapsed > cancellationCharFastKillBudget {
		t.Errorf("%s returned in %v after cancellation, want under %v: the process-group kill did not fire promptly, only the WaitDelay fallback closed the pipes", opName, elapsed, cancellationCharFastKillBudget)
	}
}

// waitForFile is the synchronization point: a test cancels only once the
// child has actually started, instead of guessing a wall-clock deadline.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(cancellationCharKillBudget)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitForFile: %s did not appear within %v", path, cancellationCharKillBudget)
}

// hungChildScript signals start via `started`; `marker` appears only if left
// to finish, so its absence is what proves the kill.
func hungChildScript(started, marker, trailing string) string {
	script := fmt.Sprintf("touch '%s'; sleep %d; touch '%s'", started, int(cancellationCharChildSleep/time.Second), marker)
	if trailing != "" {
		script += "; " + trailing
	}
	return script
}

// `set -m` backgrounds sleep into a new process group a group-wide kill
// can't reach — a POSIX builtin, unlike `setsid`, which macOS lacks.
// `started` is written only after backgrounding, so a cancel synced on it
// can't race ahead of the escape.
func escapedGrandchildScript(started, marker string) string {
	return fmt.Sprintf("set -m; sleep %d & touch '%s'; wait; touch '%s'; echo '{}'", int(cancellationCharChildSleep/time.Second), started, marker)
}

func TestCharacterization_RunSetup_CancelledContext_GrandchildInAnotherProcessGroup_StillBoundedByWaitDelay(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	marker := filepath.Join(dir, "marker")
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: escapedGrandchildScript(started, marker)}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{}
	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSetup(goCtx, plan.Run, SessionVars{Name: "x", WorkspaceDirPath: dir}, tasks, nil)
	}()

	waitForFile(t, started)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("RunSetup: want an error surfaced from the cancelled context, got nil")
		}
	case <-time.After(cancellationCharKillBudget):
		t.Fatalf("RunSetup did not return within %v of cancellation despite effect.CancelWaitDelay=%v bounding the fallback", cancellationCharKillBudget, effect.CancelWaitDelay)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("marker file exists: the shell resumed past wait despite being cancelled")
	}
}

func TestCharacterization_RunSetup_CancelledContextKillsHungChild(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	marker := filepath.Join(dir, "marker")
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: hungChildScript(started, marker, "echo '{}'")}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{}
	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSetup(goCtx, plan.Run, SessionVars{Name: "x", WorkspaceDirPath: dir}, tasks, nil)
	}()

	waitForFile(t, started)
	cancelTime := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("RunSetup: want an error surfaced from the cancelled context, got nil")
		}
		assertGroupKillFast(t, time.Since(cancelTime), "RunSetup")
	case <-time.After(cancellationCharKillBudget):
		t.Fatalf("RunSetup did not return within %v of cancellation", cancellationCharKillBudget)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("marker file exists: the child ran to completion despite the context being cancelled")
	}
}

func TestCharacterization_RunCleanup_CancelledContextKillsHungChild(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	marker := filepath.Join(dir, "marker")
	plan := buildPlan(t,
		[]taskStub{{id: "a", scope: "run", setup: "echo '{}'", cleanup: hungChildScript(started, marker, "")}},
		[]nodeStub{{id: "a"}},
	)
	tasks := map[string]*contract.TaskState{
		"a": {Scope: "run", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	}
	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunCleanup(goCtx, plan.Run, SessionVars{Name: "x", WorkspaceDirPath: dir}, tasks, nil)
	}()

	waitForFile(t, started)
	cancelTime := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("RunCleanup: want an error surfaced from the cancelled context, got nil")
		}
		assertGroupKillFast(t, time.Since(cancelTime), "RunCleanup")
	case <-time.After(cancellationCharKillBudget):
		t.Fatalf("RunCleanup did not return within %v of cancellation", cancellationCharKillBudget)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("marker file exists: the child ran to completion despite the context being cancelled")
	}
}

func TestCharacterization_RunAliveProbe_CancelledContextKillsHungChild(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	marker := filepath.Join(dir, "marker")
	session := SessionVars{Name: "x", WorkspaceDirPath: dir}
	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunAliveProbe(goCtx, Probe{Action: shellStub(hungChildScript(started, marker, ""))}, session)
	}()

	waitForFile(t, started)
	cancelTime := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("RunAliveProbe: want an error surfaced from the cancelled context, got nil")
		}
		assertGroupKillFast(t, time.Since(cancelTime), "RunAliveProbe")
	case <-time.After(cancellationCharKillBudget):
		t.Fatalf("RunAliveProbe did not return within %v of cancellation", cancellationCharKillBudget)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("marker file exists: the child ran to completion despite the context being cancelled")
	}
}

func TestCharacterization_RunCapture_CancelledContextKillsHungChild(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	marker := filepath.Join(dir, "marker")
	session := SessionVars{Name: "x", WorkspaceDirPath: dir}
	goCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	binding := &TerminalBinding{Ops: &config.TerminalConfig{Capture: shellStub(hungChildScript(started, marker, "echo done"))}}
	type captureResult struct {
		err error
	}
	resultCh := make(chan captureResult, 1)
	go func() {
		_, err := RunCapture(goCtx, binding, session)
		resultCh <- captureResult{err: err}
	}()

	waitForFile(t, started)
	cancelTime := time.Now()
	cancel()

	select {
	case res := <-resultCh:
		if res.err == nil {
			t.Fatalf("RunCapture: want an error surfaced from the cancelled context, got nil")
		}
		assertGroupKillFast(t, time.Since(cancelTime), "RunCapture")
	case <-time.After(cancellationCharKillBudget):
		t.Fatalf("RunCapture did not return within %v of cancellation", cancellationCharKillBudget)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("marker file exists: the child ran to completion despite the context being cancelled")
	}
}
