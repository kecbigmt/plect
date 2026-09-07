package pluginservice

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kecbigmt/plecture/app/internal/datahome"
)

// runProcess starts decl's executable and blocks until it exits or ctx is
// cancelled. It records the running pid via onStart before waiting, so a
// caller observes "running" status for the whole process lifetime, not just
// after the fact. On ctx cancellation, the child's whole process group gets
// SIGTERM (a service may spawn helpers of its own); WaitDelay bounds how
// long Wait keeps waiting for the group to actually exit before the
// os/exec package escalates to SIGKILL, so a service that ignores SIGTERM
// cannot hang resident shutdown indefinitely.
//
// Child stdout/stderr are forwarded to logger, one log record per line,
// each tagged with the service id and stream — see
// docs/design/plugin-packaging.md: "Service logs are attached to the
// resident process log with the service id." They never reach the durable
// per-session event log; that log is for plect's own events, not a plugin
// daemon's arbitrary output.
func runProcess(ctx context.Context, decl Declaration, logger *slog.Logger, waitDelay time.Duration, onStart func(pid int)) error {
	cmd := exec.CommandContext(ctx, decl.ExecPath, decl.Args...)
	cmd.Env = buildEnv(decl.Env)
	stdout := &serviceLogWriter{logger: logger, id: decl.ID, stream: "stdout"}
	stderr := &serviceLogWriter{logger: logger, id: decl.ID, stream: "stderr"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		return err
	}
	onStart(cmd.Process.Pid)
	err := cmd.Wait()
	// A child that exits without a trailing newline on its last write would
	// otherwise lose that line: Write only logs on '\n' boundaries.
	stdout.flush()
	stderr.flush()
	return err
}

// serviceLogWriter is an io.Writer that logs each complete line written to
// it as one log record tagged with the owning service's id and stream
// name, buffering an incomplete trailing line across Write calls (a child
// process's stdout/stderr writes are not guaranteed to land one line per
// Write). Not safe for concurrent use — runProcess gives stdout and stderr
// each their own instance, and os/exec never calls a Writer from more than
// one goroutine at a time for a given stream.
type serviceLogWriter struct {
	logger *slog.Logger
	id     string
	stream string
	buf    bytes.Buffer
}

func (w *serviceLogWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// No full line yet: ReadString still drained the partial data
			// into line, leaving buf empty — put it back for the next
			// Write (or flush) to complete.
			w.buf.WriteString(line)
			break
		}
		w.logLine(strings.TrimSuffix(line, "\n"))
	}
	return len(p), nil
}

// flush logs a trailing line that never got a '\n', once the child has
// exited and no further Write is coming.
func (w *serviceLogWriter) flush() {
	if w.buf.Len() == 0 {
		return
	}
	w.logLine(w.buf.String())
	w.buf.Reset()
}

func (w *serviceLogWriter) logLine(line string) {
	if line == "" {
		return
	}
	w.logger.Info(line, "service", w.id, "stream", w.stream)
}

// buildEnv merges decl.Env on top of the supervisor's own environment,
// de-duplicated and sorted so a key declared in both places has one
// unambiguous value regardless of libc environ merge behavior. Secrets
// (e.g. a chat platform's bot token) are expected to already be present in the
// supervisor's own environment — plugin.toml's `env` field carries only
// non-secret literals, per the plugin service lifecycle ADR.
//
// The supervisor's own PLECT_DATA_HOME (see the datahome package) is
// dropped before overrides apply, so a plect-specific data-home relocation
// active in the resident process is not silently inherited by a long-lived
// service unless plugin.toml's own `env` table rebinds it explicitly.
// XDG_DATA_HOME is left alone: an existing service (e.g. github-watcher)
// may already depend on inheriting it for its own unrelated on-disk state —
// see datahome.InheritableEnv's doc comment.
func buildEnv(overrides map[string]string) []string {
	merged := make(map[string]string, len(overrides))
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	delete(merged, datahome.EnvVar)
	for k, v := range overrides {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + merged[k]
	}
	return out
}

// missingRequiredEnv returns the subset of required not already set in the
// supervisor's own environment.
func missingRequiredEnv(required []string) []string {
	var missing []string
	for _, name := range required {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}
