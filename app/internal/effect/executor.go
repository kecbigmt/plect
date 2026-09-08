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
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/kecbigmt/plecture/app/internal/datahome"
	"github.com/kecbigmt/plecture/app/internal/lang"
)

// CancelWaitDelay bounds how long a cancelled host invocation's Wait keeps
// listening for its stdout/stderr pipes to close on their own before forcing
// them shut (see hostExecutor.Run's Cancel/WaitDelay doc). Exported so a
// characterization test can size its own timeout budget as this value plus a
// margin, rather than hard-coding a duration that has to be kept in sync by
// hand with the one below.
const CancelWaitDelay = 2 * time.Second

const (
	textBusyMaxAttempts = 5
	textBusyBaseBackoff = 5 * time.Millisecond
)

// ExecRequest is a single host-process invocation: Argv[0] is the command,
// Dir is the working directory (applied only if it exists, see hostExecutor),
// Stdin is optional (nil), Env is additions on top of the process's own
// environment, and IsolateDataHome picks datahome.IsolatedEnv over
// InheritableEnv as that base (see ExecHook/RunHook).
type ExecRequest struct {
	Argv            []string
	Dir             string
	Stdin           []byte
	Env             []string
	IsolateDataHome bool
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

// A concurrently forked sibling can hold this process's own just-closed script
// open for writing until its own exec finishes, so ETXTBSY here is retried.
func (hostExecutor) Run(ctx context.Context, req ExecRequest) (stdout, stderr []byte, err error) {
	return runWithTextBusyRetry(ctx, req, textBusyBaseBackoff)
}

// baseBackoff is a parameter so a test can widen the wait below past any
// race with the first exec attempt.
func runWithTextBusyRetry(ctx context.Context, req ExecRequest, baseBackoff time.Duration) (stdout, stderr []byte, err error) {
	backoff := baseBackoff
	for attempt := 1; ; attempt++ {
		var outBuf, errBuf bytes.Buffer
		err = runHostCmd(ctx, req, &outBuf, &errBuf)
		if attempt >= textBusyMaxAttempts || !errors.Is(err, syscall.ETXTBSY) {
			return outBuf.Bytes(), errBuf.Bytes(), err
		}
		if waitErr := waitBackoff(ctx, backoff); waitErr != nil {
			return outBuf.Bytes(), errBuf.Bytes(), waitErr
		}
		backoff *= 2
	}
}

// ctx's own error surfaces here, not the caller's stale ETXTBSY.
func waitBackoff(ctx context.Context, backoff time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(backoff):
		return nil
	}
}

func runHostCmd(ctx context.Context, req ExecRequest, outBuf, errBuf *bytes.Buffer) (err error) {
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	if req.Dir != "" {
		if _, statErr := os.Stat(req.Dir); statErr == nil {
			cmd.Dir = req.Dir
		}
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	base := datahome.InheritableEnv()
	if req.IsolateDataHome {
		base = datahome.IsolatedEnv()
	}
	cmd.Env = append(base, req.Env...)
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
	return cmd.Run()
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
// invocation, adding the working directory, injected env, and data-home
// isolation policy (see docs/design/sqlite-persistence.md, "Data-home
// resolution").
func requestFor(execution *lang.Execution, workDir string, env []string, isolateDataHome bool) ExecRequest {
	return ExecRequest{Argv: execution.Argv, Stdin: execution.Stdin, Dir: workDir, Env: env, IsolateDataHome: isolateDataHome}
}

// ExecHook runs one resolved execution through the swappable defaultExecutor
// rather than the pinned alwaysHostExecutor — see the two vars' docs. env
// carries a nesting chain's KEY=VALUE additions; empty for a plain task.
func ExecHook(ctx context.Context, execution *lang.Execution, workDir string, env ...string) (stdout, stderr []byte, err error) {
	return defaultExecutor.Run(ctx, requestFor(execution, workDir, env, true))
}

// RunHook runs one resolved execution on the host, unconditionally: the path
// workspace provider setup/cleanup, provider subscribe, and resource
// observe/finalize take.
func RunHook(ctx context.Context, execution *lang.Execution, workDir string) (stdout, stderr []byte, err error) {
	return alwaysHostExecutor.Run(ctx, requestFor(execution, workDir, nil, false))
}
