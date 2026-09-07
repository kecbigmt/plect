package effect

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/lang"
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

// hostExecutor.Run used to leave cmd.Env nil with an empty ExecRequest.Env,
// inheriting PLECT_DATA_HOME verbatim regardless of IsolateDataHome.
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

// IsolateDataHome=true (ExecHook's policy, used for a task's setup/cleanup)
// must strip XDG_DATA_HOME too, not just PLECT_DATA_HOME.
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

// ExecHook is the real entry point a task's setup/cleanup (a
// terminal-multiplexer pane's setup, in particular) runs through, so this
// pins the acceptance-level guarantee: neither data-home variable reaches
// that child.
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

// RunHook backs a workspace provider or resource observer, which may
// already depend on inheriting XDG_DATA_HOME for its own on-disk state.
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
