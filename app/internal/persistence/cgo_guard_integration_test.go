//go:build integration

package persistence

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCGOGuard_DisabledBuildFailsWithActionableMessage is the standing
// proof, run in CI's integration-tagged job, that a CGO_ENABLED=0 core
// build fails at compile time rather than silently linking
// github.com/mattn/go-sqlite3's cgo-free stub driver (which compiles but
// cannot open a database). It shells out to `go build` because flipping
// CGO_ENABLED for an in-process test cannot affect that test binary's own,
// already-compiled build.
func TestCGOGuard_DisabledBuildFailsWithActionableMessage(t *testing.T) {
	moduleDir := moduleRootDir(t)

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = moduleDir
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("CGO_ENABLED=0 go build ./... unexpectedly succeeded:\n%s", out)
	}
	const wantMessage = "plect_core_requires_CGO_ENABLED_1_set_CGO_ENABLED_1_or_build_with_a_C_toolchain"
	if !strings.Contains(string(out), wantMessage) {
		t.Fatalf("CGO_ENABLED=0 go build ./... failed, but not via the expected cgo guard (internal/persistence/cgo_required.go); output:\n%s", out)
	}
}

func moduleRootDir(t *testing.T) string {
	t.Helper()
	// Not `go list -m -f {{.Dir}}` with no module argument: the repository's
	// go.work puts every module in the workspace in scope, so that would
	// list every module's directory, not just this one's.
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}
