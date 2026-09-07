package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/lang"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// livenessTestResolverFields is a synthetic resolver pair, deliberately
// independent of any real provider's identifier shape: this suite's subject
// is the liveness check itself, not a resource dispatch convention.
const livenessTestResolverFields = `match = '^plect-test://(?P<id>[a-z0-9-]+)$'
name  = { from = "match.id" }
`

// providerRealAliveWorkspace is a resolver-backed provider whose setup
// creates workspaceDir and whose [health.alive] actually checks for it
// (`test -d`), rather than the noop most fixtures use — the subject here is
// the liveness check itself, not what it gates. cleanup touches
// cleanupMarker so a test can tell whether repair ran.
func providerRealAliveWorkspace(id, workspaceDir, cleanupMarker string) string {
	setupScript := fmt.Sprintf("mkdir -p %s\nprintf '{\"workspace_dir\":\"%s\"}'\n", workspaceDir, workspaceDir)
	cleanupScript := fmt.Sprintf("touch %s\n", cleanupMarker)
	return providerDoc(id, livenessTestResolverFields, `[%[1]s.setup]
type    = "exec"
command = "sh"
args    = ["-c", `+fmt.Sprintf("%q", setupScript)+`, "provider"]

[%[1]s.health.alive]
type   = "shell"
script = 'test -d "$workspace_dir"'

[%[1]s.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }

[%[1]s.cleanup]
type    = "exec"
command = "sh"
args    = ["-c", `+fmt.Sprintf("%q", cleanupScript)+`, "provider"]

[%[1]s.outputs_schema]
type = "object"

[%[1]s.outputs_schema.properties]
workspace_dir = { type = "string" }
`)
}

// providerNoopAliveWorkspace mirrors providerRealAliveWorkspace's setup but
// declares an explicit noop [health.alive]: the produced record is reused
// without ever checking whether workspaceDir still exists.
func providerNoopAliveWorkspace(id, workspaceDir string) string {
	setupScript := fmt.Sprintf("mkdir -p %s\nprintf '{\"workspace_dir\":\"%s\"}'\n", workspaceDir, workspaceDir)
	return providerDoc(id, livenessTestResolverFields, `[%[1]s.setup]
type    = "exec"
command = "sh"
args    = ["-c", `+fmt.Sprintf("%q", setupScript)+`, "provider"]

[%[1]s.health.alive]
type = "noop"
`)
}

// boundNodeInputs is the wiring a workflow node needs to make a fixture task
// bound to the provider's rebuilt workspace_dir output, the fact
// InvalidateProviderBoundNodes watches for.
func boundNodeInputs() map[string]*lang.Value {
	return map[string]*lang.Value{"wd": {Form: lang.FormFrom, From: "workflow.outputs.workspace_dir"}}
}

// Given a produced session whose workspace_dir directory was removed, when
// plect up runs, then the provider's alive fails, cleanup(force) and setup
// run, the directory exists again, and nodes bound to workspace_dir (and
// their dependents) are cleaned and rebuilt.
func TestUp_VanishedWorkspaceRepairsProviderAndInvalidatesBoundNodes(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cleanupMarker := filepath.Join(t.TempDir(), "provider-cleaned")
	boundMarker := filepath.Join(t.TempDir(), "bound-setup-ran")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "bound", scope: "session", setup: "touch " + boundMarker + "; echo '{}'"}},
		[]nodeFixture{{id: "bound", inputs: boundNodeInputs()}})
	writeSetupWorkflow(t, cfg, "wf", providerRealAliveWorkspace("wf", workdir, cleanupMarker))

	url := "plect-test://case-1"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionName := "case-1+wf"
	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if _, statErr := os.Stat(boundMarker); statErr != nil {
		t.Fatal("bound node's setup did not run on first up")
	}
	if err := os.Remove(boundMarker); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(workdir); err != nil {
		t.Fatal(err)
	}

	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("Up after vanished workspace: %v", err)
	}
	if _, statErr := os.Stat(workdir); statErr != nil {
		t.Fatal("the workspace directory must exist again after repair")
	}
	if _, statErr := os.Stat(cleanupMarker); statErr != nil {
		t.Fatal("provider cleanup must run as part of the repair")
	}
	if _, statErr := os.Stat(boundMarker); statErr != nil {
		t.Fatal("bound, whose input reads workflow.outputs.workspace_dir, must be rebuilt after provider repair")
	}
	s := store.Get(sessionName)
	if st := s.Tasks["bound"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("bound node state = %+v, want produced after rebuild", st)
	}
	if st := s.Tasks[contract.WorkflowPseudoNodeID]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("pseudo-node state = %+v, want produced after repair", st)
	}
}

// Given a produced session whose workspace is intact, when plect up runs,
// then the alive action passes, provider cleanup does not run, and
// uncommitted files in the workspace are untouched.
func TestUp_IntactWorkspaceReusesProviderWithoutCleanup(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")
	cleanupMarker := filepath.Join(t.TempDir(), "provider-cleaned")
	boundMarker := filepath.Join(t.TempDir(), "bound-setup-ran")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "bound", scope: "session", setup: "touch " + boundMarker + "; echo '{}'"}},
		[]nodeFixture{{id: "bound", inputs: boundNodeInputs()}})
	writeSetupWorkflow(t, cfg, "wf", providerRealAliveWorkspace("wf", workdir, cleanupMarker))

	url := "plect-test://case-2"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionName := "case-2+wf"
	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := os.Remove(boundMarker); err != nil {
		t.Fatal(err)
	}
	uncommitted := filepath.Join(workdir, "uncommitted.txt")
	if err := os.WriteFile(uncommitted, []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if _, statErr := os.Stat(cleanupMarker); statErr == nil {
		t.Fatal("provider cleanup ran even though the workspace was intact")
	}
	if _, statErr := os.Stat(uncommitted); statErr != nil {
		t.Fatal("uncommitted work in the still-live workspace must survive an ordinary up")
	}
	if _, statErr := os.Stat(boundMarker); statErr == nil {
		t.Fatal("bound node's setup re-ran even though the provider was never repaired")
	}
	s := store.Get(sessionName)
	if st := s.Tasks["bound"]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("bound node state = %+v, want left untouched (produced)", st)
	}
}

// Given a provider alive declared as noop, when plect up runs, then the
// produced record is reused without executing anything — not even a
// directory check.
func TestUp_NoopAliveReusesProducedRecordWithoutExecutingAnything(t *testing.T) {
	store := testStore(t)
	workdir := filepath.Join(t.TempDir(), "wd")

	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "noop", scope: "session", setup: "echo '{}'"}},
		[]nodeFixture{{id: "noop"}})
	writeSetupWorkflow(t, cfg, "wf", providerNoopAliveWorkspace("wf", workdir))

	url := "plect-test://case-3"
	if _, err := Create(cfg, store, CreateParams{URL: url}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sessionName := "case-3+wf"
	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("first Up: %v", err)
	}

	// A noop probe never observes the surface, so the record is reused even
	// though the directory it named is gone.
	if err := os.RemoveAll(workdir); err != nil {
		t.Fatal(err)
	}
	if _, err := Up(cfg, store, UpParams{Identifier: sessionName}); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if _, statErr := os.Stat(workdir); statErr == nil {
		t.Fatal("a noop alive probe must never re-run setup, so the vanished directory must not come back")
	}
	s := store.Get(sessionName)
	if st := s.Tasks[contract.WorkflowPseudoNodeID]; st == nil || st.Status != contract.TaskStatusProduced {
		t.Fatalf("pseudo-node state = %+v, want produced (reused)", st)
	}
}
