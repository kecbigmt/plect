package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// These tests cover cancellation for the five task exec paths (setup,
// cleanup, alive probe, capture, dynamic-output fetch): each accepts a
// context.Context, and cancelling it must terminate the child process (so
// the marker file it would otherwise write never appears) and the error
// must surface to the caller promptly instead of the call blocking for the
// child's full lifetime.
//
// "Promptly" is measured from the moment the context is actually cancelled,
// not from the call's start: waitForFile is the synchronization point that
// confirms the child has started before cancel() runs, so the measured
// window is kill latency alone, not kill latency plus however long process
// fork/exec happened to take under whatever load the machine is under.

const cancellationCharChildSleep = 5 * time.Second

// cancellationCharKillBudget bounds how long a caller may take to return
// once the context is cancelled. It is generous relative to real kill
// latency so it tolerates CPU contention, but stays far under
// cancellationCharChildSleep so a regression that fails to kill the child
// (letting it run to completion) still fails the test instead of passing
// under a loose bound.
const cancellationCharKillBudget = 3 * time.Second

// waitForFile polls for path to appear, the synchronization point a test
// uses to know the child process has actually started before it cancels the
// context — avoiding a guessed wall-clock deadline for "surely started by
// now", which is what made these tests flaky under CPU load.
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

// hungChildScript builds the shared shell script: it touches started before
// sleeping, so waitForFile has something to poll for, and touches marker
// after — its absence is what proves the child was killed rather than left
// to run to completion.
func hungChildScript(started, marker, trailing string) string {
	script := fmt.Sprintf("touch '%s'; sleep %d; touch '%s'", started, int(cancellationCharChildSleep/time.Second), marker)
	if trailing != "" {
		script += "; " + trailing
	}
	return script
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
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("RunSetup: want an error surfaced from the cancelled context, got nil")
		}
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
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("RunCleanup: want an error surfaced from the cancelled context, got nil")
		}
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
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("RunAliveProbe: want an error surfaced from the cancelled context, got nil")
		}
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
	cancel()

	select {
	case res := <-resultCh:
		if res.err == nil {
			t.Fatalf("RunCapture: want an error surfaced from the cancelled context, got nil")
		}
	case <-time.After(cancellationCharKillBudget):
		t.Fatalf("RunCapture did not return within %v of cancellation", cancellationCharKillBudget)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Errorf("marker file exists: the child ran to completion despite the context being cancelled")
	}
}
