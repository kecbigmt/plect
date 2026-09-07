//go:build integration

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kecbigmt/plecture/app/internal/state"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// TestIntegration_TickSpawnsReviewer drives the full fire path: a work
// session with a pending judge leaf and green checks, a task-embedded
// [[chains]] that wires `revision`, and plect tick (TickSession) spawning the
// reviewer workflow as a sibling under the work session's parent as part of
// the same tick that advances the Goal Loop. Re-ticking is idempotent.
func TestIntegration_TickSpawnsReviewer(t *testing.T) {
	workdirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workdirsRoot, "default",
		[]taskFixture{
			{
				id: "work",
				extra: `
[done_when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge = "ac met", id = "ac-met" },
]

[[chains]]
id       = "review"
workflow = "default"
resource = { from = "resource.state.pr_url" }
[chains.when]
all = [
  { judge_pending = "ac-met" },
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
]
[chains.inputs]
revision = { from = "resource.state.revision" }
`,
			},
			{id: "tmux", scope: "run", setup: `echo '{"session_name":"t"}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "tmux"}},
	)
	// The work session is keyed to an issue; the pull request it opened is a
	// fact its resource observation reports, and the chain names it.
	const prURL = "https://github.com/testowner/testrepo/pull/56"
	stubObservedFacts(t, cfg, ".", map[string]any{"checks_status": "SUCCESS", "revision": "sha1", "pr_url": prURL})

	url := "https://github.com/testowner/testrepo/issues/55"
	work := "testowner/testrepo-55+default"
	reviewer := "testowner/testrepo-56+review-work"

	if _, err := Up(cfg, store, UpParams{Identifier: url}); err != nil {
		t.Fatalf("Up(work): %v", err)
	}
	// A workflow builds the session; the work a chain fires off is a task
	// document dispatched into it. --name keeps the instance key stable, so
	// the spawn tag a fire derives from it is too.
	if _, err := TaskSetup(cfg, store, TaskSetupParams{TaskID: "work", SessionName: work, Name: "work", Resource: url}); err != nil {
		t.Fatalf("TaskSetup(work): %v", err)
	}
	// Parent the work session so the sibling reviewer is tree-attached.
	seedSession(t, store, "testowner/testrepo-orch", "testowner/testrepo", 0, "", nil)
	setParent(t, store, work, "testowner/testrepo-orch")

	res, err := TickSession(cfg, store, TickParams{SessionName: work, SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	sp, ok := findSpawn(res.Chains, "review")
	if !ok || !sp.Fired || !sp.Spawned {
		t.Fatalf("expected fired+spawned, got %+v", res.Chains)
	}
	if sp.TargetSession != reviewer {
		t.Fatalf("target = %q, want %q", sp.TargetSession, reviewer)
	}
	spawned := store.Get(reviewer)
	if spawned == nil {
		t.Fatalf("reviewer session %q not created", reviewer)
	}
	if spawned.ParentSession != "testowner/testrepo-orch" {
		t.Fatalf("reviewer parent = %q, want orchestrator", spawned.ParentSession)
	}
	if spawned.Workflow != "default" {
		t.Fatalf("reviewer workflow = %q, want default", spawned.Workflow)
	}
	// The reviewer is about the pull request the chain named, not the issue
	// the work session is keyed to.
	if spawned.ResourceID != prURL {
		t.Fatalf("reviewer resource = %q, want the pull request the chain named (%q)", spawned.ResourceID, prURL)
	}
	// The reviewer session is a real session the workflow built, not a
	// record: its nodes ran.
	if e := spawned.Nodes["tmux"]; e == nil || e.Status != contract.TaskStatusProduced {
		t.Fatalf("reviewer tmux node = %v, want produced", e)
	}
	// The wired `revision` binding is rendered from the work outputs and fed to
	// the spawned reviewer as a session input.
	if spawned.Inputs["revision"] != "sha1" {
		t.Fatalf("reviewer revision input = %v, want sha1", spawned.Inputs["revision"])
	}

	// Idempotent: a second tick finds the reviewer already active, no re-spawn.
	res2, err := TickSession(cfg, store, TickParams{SessionName: work, SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	sp2, _ := findSpawn(res2.Chains, "review")
	if !sp2.Fired || !sp2.AlreadyActive || sp2.Spawned {
		t.Fatalf("expected already-active without re-spawn, got %+v", sp2)
	}
}

// TestIntegration_LegacyChainsFileIsRefused is the dual-read retirement
// counterpart to TestIntegration_TickSpawnsReviewer: a chains/*.toml
// declaration is not a definition, and a definition root admits none, so the
// whole layer refuses to load rather than quietly not firing the rule.
func TestIntegration_LegacyChainsFileIsRefused(t *testing.T) {
	workdirsRoot := setupE2ERepo(t)
	setupFakeScripts(t)
	store := state.NewStore(t.TempDir())

	cfg := writeIntegrationFixture(t, workdirsRoot, "default",
		[]taskFixture{
			{
				id:    "work",
				extra: "[done_when]\nall = [\n  { check = \"resource.state.checks_status\", in = [\"SUCCESS\"] },\n  { judge = \"ac met\", id = \"ac-met\" },\n]\n",
			},
			{id: "tmux", scope: "run", setup: `echo '{"session_name":"t"}'`, cleanup: "true"},
		},
		[]nodeFixture{{id: "tmux"}},
	)
	stubObservedFacts(t, cfg, ".", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	dir := filepath.Join(cfg.BaseDir, "chains")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "review.toml"), []byte(`
[[chains]]
id       = "review"
workflow = "default"
[chains.when]
all = [
  { judge_pending = "ac-met" },
  { check = "checks_status", in = ["SUCCESS"] },
]
[chains.inputs]
revision = "{{.Work.outputs.revision}}"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Up(cfg, store, UpParams{Identifier: "https://github.com/testowner/testrepo/issues/55"}); err == nil {
		t.Fatal("expected the load to refuse the leftover chains/*.toml")
	} else if !strings.Contains(err.Error(), "review.toml") {
		t.Fatalf("err = %v, want the error to name the leftover file", err)
	}
}
