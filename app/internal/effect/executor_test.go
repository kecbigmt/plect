package effect

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/plectshim"
)

// shellExecution is the invocation a `bash -c` script resolves to, stated
// here because the tests that assert on the host path's own shape are the
// only remaining callers that need to build one by hand.
func shellExecution(script string) *lang.Execution {
	return &lang.Execution{Argv: []string{"bash", "-c", script}}
}

// A host invocation's two long-standing shape rules: stdout and stderr are
// captured separately, and a working directory that does not exist is
// ignored rather than surfaced as an error.
func TestExecutor_HostExecutorCapturesSeparatelyAndIgnoresAMissingDir(t *testing.T) {
	var exec Executor = hostExecutor{}
	stdout, stderr, err := exec.Run(context.Background(), ExecRequest{Argv: []string{"bash", "-c", `echo out; echo err >&2`}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stdout) != "out\n" || string(stderr) != "err\n" {
		t.Errorf("stdout=%q stderr=%q", stdout, stderr)
	}

	// A Dir that doesn't exist must be silently ignored, not surfaced as an
	// error: a hook that runs before its workspace exists still has to run.
	stdout, _, err = exec.Run(context.Background(), ExecRequest{Argv: []string{"bash", "-c", "pwd"}, Dir: "/nonexistent/does-not-exist"})
	if err != nil {
		t.Fatalf("Run with missing Dir: %v", err)
	}
	if string(stdout) == "/nonexistent/does-not-exist\n" {
		t.Errorf("Dir was applied despite not existing")
	}
}

func TestExecutor_RequestForKeepsEachFormsInvocationShape(t *testing.T) {
	tests := []struct {
		name            string
		execution       *lang.Execution
		workDir         string
		env             []string
		isolateDataHome bool
		want            ExecRequest
	}{
		{
			name:      "a shell execution",
			execution: shellExecution(`echo '{}'`),
			workDir:   "/work/x",
			want:      ExecRequest{Argv: []string{"bash", "-c", `echo '{}'`}, Dir: "/work/x"},
		},
		{
			name:      "a shell execution carrying an enclosing layer's env",
			execution: shellExecution(`echo hi`),
			workDir:   "/work/x",
			env:       []string{"PLECT_GUARD=on"},
			want:      ExecRequest{Argv: []string{"bash", "-c", `echo hi`}, Dir: "/work/x", Env: []string{"PLECT_GUARD=on"}},
		},
		{
			name:      "a resolved action",
			execution: &lang.Execution{Argv: []string{"/plugins/bin/okf-goal", "resource", "finalize"}, Stdin: []byte(`[]`)},
			want:      ExecRequest{Argv: []string{"/plugins/bin/okf-goal", "resource", "finalize"}, Stdin: []byte(`[]`)},
		},
		{
			name:            "a task setup execution isolates its data home",
			execution:       shellExecution(`echo hi`),
			isolateDataHome: true,
			want:            ExecRequest{Argv: []string{"bash", "-c", `echo hi`}, IsolateDataHome: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requestFor(tt.execution, tt.workDir, tt.env, tt.isolateDataHome)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("requestFor = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExecutor_HostExecutorStripsPlectDataHomeUnlessEnvRebindsIt(t *testing.T) {
	t.Setenv("PLECT_DATA_HOME", "/poisoned")
	t.Setenv("XDG_DATA_HOME", "/still-inherited")
	t.Setenv("UNRELATED_VAR", "kept")

	var exec Executor = hostExecutor{}
	stdout, _, err := exec.Run(context.Background(), ExecRequest{Argv: []string{"env"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := string(stdout)
	if strings.Contains(got, "PLECT_DATA_HOME=") {
		t.Fatalf("child env leaked PLECT_DATA_HOME:\n%s", got)
	}
	if !strings.Contains(got, "XDG_DATA_HOME=/still-inherited") {
		t.Fatalf("IsolateDataHome=false child env dropped XDG_DATA_HOME, want it still inherited:\n%s", got)
	}
	if !strings.Contains(got, "UNRELATED_VAR=kept") {
		t.Fatalf("child env dropped an unrelated variable:\n%s", got)
	}

	// A layer's own explicit binding must still win over the strip.
	stdout, _, err = exec.Run(context.Background(), ExecRequest{Argv: []string{"env"}, Env: []string{"PLECT_DATA_HOME=/explicit"}})
	if err != nil {
		t.Fatalf("Run with explicit Env: %v", err)
	}
	if !strings.Contains(string(stdout), "PLECT_DATA_HOME=/explicit") {
		t.Fatalf("an explicit Env binding did not survive the strip:\n%s", stdout)
	}
}

func TestExecutor_HostExecutorStripsPlectCacheHomeButKeepsXDGCacheHome(t *testing.T) {
	t.Setenv("PLECT_CACHE_HOME", "/poisoned-cache")
	t.Setenv("XDG_CACHE_HOME", "/still-inherited-cache")

	var exec Executor = hostExecutor{}
	stdout, _, err := exec.Run(context.Background(), ExecRequest{Argv: []string{"env"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := string(stdout)
	if strings.Contains(got, "PLECT_CACHE_HOME=") {
		t.Fatalf("child env leaked PLECT_CACHE_HOME:\n%s", got)
	}
	if !strings.Contains(got, "XDG_CACHE_HOME=/still-inherited-cache") {
		t.Fatalf("child env dropped XDG_CACHE_HOME, want it still inherited:\n%s", got)
	}

	// IsolateDataHome=true isolates the cache-home vars the same way it does
	// the data-home ones: both PLECT_ and XDG_ get stripped.
	stdout, _, err = exec.Run(context.Background(), ExecRequest{Argv: []string{"env"}, IsolateDataHome: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got = string(stdout)
	if strings.Contains(got, "PLECT_CACHE_HOME=") || strings.Contains(got, "XDG_CACHE_HOME=") {
		t.Fatalf("child env leaked a cache-home variable with IsolateDataHome=true:\n%s", got)
	}
}

func TestExecutor_HostExecutorIsolatesXDGDataHomeWhenRequested(t *testing.T) {
	t.Setenv("PLECT_DATA_HOME", "/poisoned")
	t.Setenv("XDG_DATA_HOME", "/poisoned-xdg")

	var exec Executor = hostExecutor{}
	stdout, _, err := exec.Run(context.Background(), ExecRequest{Argv: []string{"env"}, IsolateDataHome: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := string(stdout)
	if strings.Contains(got, "PLECT_DATA_HOME=") || strings.Contains(got, "XDG_DATA_HOME=") {
		t.Fatalf("IsolateDataHome=true child env leaked a data-home variable:\n%s", got)
	}
}

func TestExecHook_IsolatesBothDataHomeVars(t *testing.T) {
	t.Setenv("PLECT_DATA_HOME", "/poisoned")
	t.Setenv("XDG_DATA_HOME", "/poisoned-xdg")

	stdout, _, err := ExecHook(context.Background(), &lang.Execution{Argv: []string{"env"}}, "")
	if err != nil {
		t.Fatalf("ExecHook: %v", err)
	}
	if strings.Contains(string(stdout), "PLECT_DATA_HOME=") || strings.Contains(string(stdout), "XDG_DATA_HOME=") {
		t.Fatalf("ExecHook leaked a data-home variable:\n%s", stdout)
	}
}

// plect-web (webui.LiveService.Up) reaches this same ExecHook path but has
// no CLI subcommand tree of its own, so the fake standing in for it here
// fails loud if the shim ever executes it directly instead of its "plect"
// sibling -- the failure a real pane would see if plectshim picked wrong.
func TestExecHook_BarePlectReachesTheDaemonStoreThroughTheShim(t *testing.T) {
	dataHome := t.TempDir()
	hostDir := t.TempDir()
	webBin := filepath.Join(hostDir, "plect-web")
	if err := os.WriteFile(webBin, []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "plect"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer plectshim.UseExecutableForTest(webBin)()
	t.Setenv("PLECT_DATA_HOME", dataHome)
	// Cleared, not left ambient: a CI runner's own XDG_CONFIG_HOME would
	// otherwise leak into the shim script this test asserts on verbatim.
	t.Setenv("PLECT_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("PLECT_CACHE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	stdout, _, err := ExecHook(context.Background(), shellExecution("plect ls --json"), "")
	if err != nil {
		t.Fatalf("ExecHook: %v", err)
	}
	want := "--data-home\n" + dataHome + "\nls\n--json\n"
	if string(stdout) != want {
		t.Fatalf("plect ls --json, forwarded via the shim = %q, want %q (the daemon's own store)", stdout, want)
	}
}

func TestExecHook_DotSlashPlectBypassesTheShimAndStaysIsolated(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("PLECT_DATA_HOME", dataHome)

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "plect"), []byte("#!/bin/sh\nenv\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := ExecHook(context.Background(), shellExecution("./plect"), work)
	if err != nil {
		t.Fatalf("ExecHook: %v", err)
	}
	if strings.Contains(string(stdout), "PLECT_DATA_HOME=") || strings.Contains(string(stdout), "XDG_DATA_HOME=") {
		t.Fatalf("a sibling ./plect build saw a data-home variable, want it isolated like any other build the agent makes itself:\n%s", stdout)
	}
}

func TestRunHook_KeepsXDGDataHomeButIsolatesPlectDataHome(t *testing.T) {
	t.Setenv("PLECT_DATA_HOME", "/poisoned")
	t.Setenv("XDG_DATA_HOME", "/still-inherited")

	stdout, _, err := RunHook(context.Background(), &lang.Execution{Argv: []string{"env"}}, "")
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	got := string(stdout)
	if strings.Contains(got, "PLECT_DATA_HOME=") {
		t.Fatalf("RunHook leaked PLECT_DATA_HOME:\n%s", got)
	}
	if !strings.Contains(got, "XDG_DATA_HOME=/still-inherited") {
		t.Fatalf("RunHook dropped XDG_DATA_HOME, want it still inherited:\n%s", got)
	}
}

// Linux keeps exec of a path ETXTBSY as long as any process anywhere has it
// open for writing, so only a genuinely separate process — not an fd this
// test process merely holds — reproduces the race hostExecutor.Run retries.
func startBusyHolder(t *testing.T, path string) (release func()) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `exec 3>>"$0"; printf ready; read _`, path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting busy holder: %v", err)
	}
	ready := make([]byte, len("ready"))
	if _, err := io.ReadFull(stdout, ready); err != nil {
		t.Fatalf("waiting for busy holder: %v", err)
	}
	return func() {
		stdin.Close()
		_ = cmd.Wait()
	}
}

func writeExecutableScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env sh\necho ok\n"), 0o700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestExecutor_HostExecutorRetriesPastATextFileBusyRace(t *testing.T) {
	path := writeExecutableScript(t, t.TempDir())
	release := startBusyHolder(t, path)
	time.AfterFunc(2*textBusyBaseBackoff, release)

	var exec Executor = hostExecutor{}
	stdout, _, err := exec.Run(context.Background(), ExecRequest{Argv: []string{path}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stdout) != "ok\n" {
		t.Errorf("stdout = %q, want %q", stdout, "ok\n")
	}
}

// skipUnlessTextBusyEnforced skips the calling test unless the kernel keeps
// exec of a path ETXTBSY while any process holds it open for writing:
// Linux enforces this, but macOS does not, so the exec below would just
// succeed and leave nothing for the retry logic to do.
func skipUnlessTextBusyEnforced(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("ETXTBSY is Linux-specific")
	}
}

func TestExecutor_HostExecutorGivesUpAfterBoundedTextFileBusyAttempts(t *testing.T) {
	skipUnlessTextBusyEnforced(t)
	path := writeExecutableScript(t, t.TempDir())
	defer startBusyHolder(t, path)()

	start := time.Now()
	var exec Executor = hostExecutor{}
	_, _, err := exec.Run(context.Background(), ExecRequest{Argv: []string{path}})
	elapsed := time.Since(start)

	if !errors.Is(err, syscall.ETXTBSY) {
		t.Fatalf("err = %v, want ETXTBSY", err)
	}
	maxBackoff := textBusyBaseBackoff * (1 << textBusyMaxAttempts)
	if elapsed > maxBackoff {
		t.Errorf("Run took %v to give up, want well under %v (bounded retry budget)", elapsed, maxBackoff)
	}
}

// A cancelled context, not an elapsed timer, exercises exactly the branch a
// timing-based test could race past.
func TestWaitBackoff_ReturnsContextErrorOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitBackoff(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitBackoff = %v, want context.Canceled", err)
	}
}

func TestWaitBackoff_WaitsOutABackoffThatIsNotCancelled(t *testing.T) {
	if err := waitBackoff(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("waitBackoff = %v, want nil", err)
	}
}

// baseBackoff is an hour and cancellation fires 50ms in, so this can only
// land inside the backoff wait, never race the first exec attempt the way a
// deadline sized against the production backoff would.
func TestExecutor_RunWithTextBusyRetryReturnsContextErrorWhenCancelledDuringBackoff(t *testing.T) {
	skipUnlessTextBusyEnforced(t)
	path := writeExecutableScript(t, t.TempDir())
	defer startBusyHolder(t, path)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)

	_, _, err := runWithTextBusyRetry(ctx, ExecRequest{Argv: []string{path}}, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestExecutor_RunHookIsNeverRoutedThroughDefaultExecutor(t *testing.T) {
	spy := &SpyExecutor{Stdout: []byte("{}")}
	restore := UseExecutor(spy)
	defer restore()
	stdout, _, err := RunHook(context.Background(), &lang.Execution{Argv: []string{"echo", "real"}}, "")
	if err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	if string(stdout) != "real\n" {
		t.Errorf("stdout = %q, want RunHook to actually execute", stdout)
	}
	if len(spy.Requests) != 0 {
		t.Errorf("spy recorded %d requests, want 0", len(spy.Requests))
	}
}
