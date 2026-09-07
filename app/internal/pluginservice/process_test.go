package pluginservice

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunProcess_ForwardsChildOutputToLoggerWithServiceID(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "logger", `
echo "stdout marker"
echo "stderr marker" >&2
`)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	decl := Declaration{ID: "p/svc", PluginID: "p", Name: "svc", ExecPath: script}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := runProcess(ctx, decl, logger, time.Second, func(int) {}); err != nil {
		t.Fatalf("runProcess: unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "stdout marker") || !strings.Contains(out, `service=p/svc`) {
		t.Fatalf("log output missing stdout line tagged with service id, got:\n%s", out)
	}
	if !strings.Contains(out, "stderr marker") {
		t.Fatalf("log output missing stderr line, got:\n%s", out)
	}
	if !strings.Contains(out, "stream=stdout") || !strings.Contains(out, "stream=stderr") {
		t.Fatalf("log output missing stream tags, got:\n%s", out)
	}
}

// buildEnv used to copy os.Environ() verbatim, so a service inherited the
// supervisor's PLECT_DATA_HOME unconditionally. XDG_DATA_HOME must still
// reach the service — an existing one may depend on inheriting it for its
// own unrelated on-disk state.
func TestBuildEnv_StripsPlectDataHomeButKeepsXDGDataHomeUnlessRebound(t *testing.T) {
	t.Setenv("PLECT_DATA_HOME", "/poisoned")
	t.Setenv("XDG_DATA_HOME", "/still-inherited")
	t.Setenv("UNRELATED_VAR", "kept")

	env := buildEnv(nil)
	if slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "PLECT_DATA_HOME=")
	}) {
		t.Fatalf("buildEnv(nil) leaked PLECT_DATA_HOME: %v", env)
	}
	if !slices.Contains(env, "XDG_DATA_HOME=/still-inherited") {
		t.Fatalf("buildEnv(nil) dropped XDG_DATA_HOME, want it still inherited: %v", env)
	}
	if !slices.Contains(env, "UNRELATED_VAR=kept") {
		t.Fatalf("buildEnv(nil) dropped an unrelated variable: %v", env)
	}

	env = buildEnv(map[string]string{"PLECT_DATA_HOME": "/explicit"})
	if !slices.Contains(env, "PLECT_DATA_HOME=/explicit") {
		t.Fatalf("plugin.toml's own env override did not survive the strip: %v", env)
	}
}

func TestRunProcess_FlushesTrailingLineWithoutNewline(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "no-newline", `printf 'no trailing newline'`)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	decl := Declaration{ID: "p/svc2", PluginID: "p", Name: "svc2", ExecPath: script}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := runProcess(ctx, decl, logger, time.Second, func(int) {}); err != nil {
		t.Fatalf("runProcess: unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "no trailing newline") {
		t.Fatalf("log output missing the unterminated trailing line, got:\n%s", buf.String())
	}
}
