// Package effect resolves and runs actions against their surface roots, and
// owns the effect-nesting machinery a chain of layers composes: setup/
// cleanup execution, the `[id.inner]` joint, `[outputs.bind]` projection, and
// `[terminal]` lookup — effects own these, not tasks. It also owns the
// workspace-provider hooks (setup/cleanup/subscribe), which run before any
// task DAG exists and take only plain, session-identifier-shaped values as
// their context.
//
// Root-building that depends on a session's task-DAG state (node outputs,
// self.state) stays in app/internal/task, which hands this package only the
// closures a chain walk needs (ChainHost, Capabilities) — this package never
// references a RenderContext or an Observer by name.
package effect

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/kecbigmt/plecture/app/internal/lang"
)

// CancelWaitDelay bounds how long a cancelled host invocation's Wait keeps
// listening for its stdout/stderr pipes to close on their own before forcing
// them shut (see hostExecutor.Run's Cancel/WaitDelay doc). Exported so a
// characterization test can size its own timeout budget as this value plus a
// margin, rather than hard-coding a duration that has to be kept in sync by
// hand with the one below.
const CancelWaitDelay = 2 * time.Second

// ExecRequest is a single host-process invocation: Argv[0] is the command,
// Dir is the working directory (applied only if it exists, see hostExecutor),
// and Stdin/Env are optional: nil means "none" and "inherit the process
// env".
type ExecRequest struct {
	Argv  []string
	Dir   string
	Stdin []byte
	Env   []string
}

// Executor runs an ExecRequest and returns its captured stdout/stderr. The
// only implementation is the host one (see hostExecutor); the seam exists so
// tests can observe what each exec path issues. ctx governs the lifetime of
// the underlying child process: a cancelled ctx must terminate it and surface
// an error, not merely stop waiting on it.
type Executor interface {
	Run(ctx context.Context, req ExecRequest) (stdout, stderr []byte, err error)
}

// hostExecutor runs argv directly as a host process.
type hostExecutor struct{}

func (hostExecutor) Run(ctx context.Context, req ExecRequest) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if req.Dir != "" {
		if _, statErr := os.Stat(req.Dir); statErr == nil {
			cmd.Dir = req.Dir
		}
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}
	// Put the child in its own process group and, on cancellation, kill the
	// whole group rather than just the direct child. A shell script's own
	// children (e.g. "sleep 5" spawned by "bash -c") don't die with their
	// parent: exec.CommandContext's default Cancel only signals the direct
	// child, so a grandchild holding the stdout/stderr pipes open would keep
	// cmd.Wait blocked until it exits on its own, defeating cancellation
	// entirely. WaitDelay bounds how long Wait keeps waiting after Cancel runs before
	// forcibly closing the I/O pipes, so a runaway grandchild can't hang the
	// call forever even if the group kill somehow fails to reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		groupErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if groupErr == nil {
			return nil
		}
		// The child's own setpgid(2) (run between fork and exec) can lose the
		// race against an immediate cancellation, so "-pid" does not yet name
		// a process group and the group kill above returns ESRCH. Signalling
		// the pid directly still reaches the child in that narrow window.
		if directErr := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); directErr == nil {
			return nil
		}
		return groupErr
	}
	cmd.WaitDelay = CancelWaitDelay
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// alwaysHostExecutor backs RunHook, the path used by workspace provider
// setup/cleanup, workspace provider subscribe, and resource observe/finalize.
// Unlike defaultExecutor, this is never swapped, not even by tests.
var alwaysHostExecutor Executor = hostExecutor{}

// defaultExecutor backs ExecHook, the path used by task
// setup/cleanup/health probes/capture and dynamic instance setup
// (`plect task setup --resource`) — every exec point that runs inside a
// session's task DAG, static or dynamically instantiated. Tests swap
// it for a spy (see UseExecutor) to observe the ExecRequest each path
// issues without changing any exported function signature.
var defaultExecutor Executor = hostExecutor{}

// requestFor is the one place a resolved lifecycle execution becomes a host
// invocation: an action arrives already resolved into its argv and standard
// input, and this adds only the working directory and the environment the
// enclosing layers inject.
func requestFor(execution *lang.Execution, workDir string, env []string) ExecRequest {
	return ExecRequest{Argv: execution.Argv, Stdin: execution.Stdin, Dir: workDir, Env: env}
}

// ExecHook runs one resolved execution through the swappable
// defaultExecutor rather than the pinned alwaysHostExecutor — see the two
// vars' docs for why the distinction matters. env carries the KEY=VALUE
// additions the enclosing layers of a nesting chain inject into this
// execution; it is empty for every plain task.
func ExecHook(ctx context.Context, execution *lang.Execution, workDir string, env ...string) (stdout, stderr []byte, err error) {
	return defaultExecutor.Run(ctx, requestFor(execution, workDir, env))
}

// RunHook runs one resolved execution on the host, unconditionally: the path
// workspace provider setup/cleanup, provider subscribe, and resource
// observe/finalize take.
func RunHook(ctx context.Context, execution *lang.Execution, workDir string) (stdout, stderr []byte, err error) {
	return alwaysHostExecutor.Run(ctx, requestFor(execution, workDir, nil))
}
