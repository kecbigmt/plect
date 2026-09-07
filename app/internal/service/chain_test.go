package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// workTaskWithChain is a "work" task-document fixture: green-checks +
// pending-judge done_when over the instance's own recorded state, and the
// given `[[chains]]` (and any other extra TOML) appended. A chain is declared
// by the document whose instances it fires against, and nowhere else.
func workTaskWithChain(extraChain string) taskFixture {
	return taskFixture{
		id: "work",
		extra: `
[done_when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge = "ac met", id = "ac-met" },
]
` + extraChain,
	}
}

// seedReviewWork seeds one live work instance whose resource was last
// observed to report the given facts — which is where a completion check and
// a chain projection read them from.
func seedReviewWork(t *testing.T, store *state.Store, name string, observed map[string]any) {
	t.Helper()
	seedSessionWithNodes(t, store, name, "owner/repo", 1, "wf", map[string]*contract.TaskState{
		"work": {
			Scope:    contract.TaskScopeSession,
			TaskID:   "work",
			Status:   contract.TaskStatusProduced,
			Observed: &contract.ResourceObservation{State: observed, At: time.Now()},
		},
	})
}

// writeWorkflowFile writes a spawnable workflow plus the provider that backs
// it. A workflow a chain can spawn into must be provider-backed, since the
// provider is what resolves the resource to a session id and acquires the
// working directory.
func writeWorkflowFile(t *testing.T, cfg *config.Config, id, body string) {
	t.Helper()
	dir := filepath.Join(cfg.BaseDir, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	provID, prov := providerAside(id, providerEchoingOutputs(id, `{"workdir":"/tmp/x"}`))
	header := "[" + id + "]\nkind = \"workflow\"\nworkspace_provider = \"" + provID + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, id+".toml"), []byte(header+body), 0o644); err != nil {
		t.Fatal(err)
	}
	providersDir := filepath.Join(cfg.BaseDir, "workspaces")
	if err := os.MkdirAll(providersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providersDir, provID+".toml"), []byte(prov), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findSpawn(spawns []ChainSpawn, chainID string) (ChainSpawn, bool) {
	for _, sp := range spawns {
		if sp.ChainID == chainID {
			return sp, true
		}
	}
	return ChainSpawn{}, false
}

func TestCheckSession_ChainFiresWhenWhenAndOutputsHold(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [
  { judge_pending = "ac-met" },
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
]
[chains.inputs]
revision = { from = "resource.state.revision" }
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	// A parent so a sibling reviewer would be tree-attached.
	seedSessionWithNodes(t, store, "owner/repo-orch", "owner/repo", 1, "", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orch")

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	sp, ok := findSpawn(res.Chains, "review")
	if !ok {
		t.Fatalf("review chain not evaluated: %+v", res.Chains)
	}
	if !sp.Fired || sp.BlockedReason != "" {
		t.Fatalf("expected fired, got %+v", sp)
	}
	if sp.Placement != config.ChainPlacementSibling || sp.ParentSession != "owner/repo-orch" {
		t.Fatalf("placement/parent = %q/%q", sp.Placement, sp.ParentSession)
	}
	if sp.TargetSession != "owner/repo-1+review-work" {
		t.Fatalf("target = %q", sp.TargetSession)
	}
	if len(sp.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", sp.Warnings)
	}
}

// A fired chain renders its [chains.inputs] bindings against the work facts and
// attaches the resolved inputs to the spawn (passed to Up on a real spawn).
func TestCheckSession_ChainRendersAndAttachesInputs(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [
  { judge_pending = "ac-met" },
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
]
[chains.inputs]
revision     = { from = "resource.state.revision" }
work_session = { from = "task.session" }
instance     = { from = "task.instance" }
judge_ids    = { from = "task.done_when.pending_judge_ids" }
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if !sp.Fired {
		t.Fatalf("expected fired, got %+v", sp)
	}
	if sp.Inputs["revision"] != "sha1" {
		t.Errorf("revision input = %v, want sha1", sp.Inputs["revision"])
	}
	if sp.Inputs["work_session"] != "owner/repo-1" {
		t.Errorf("work_session input = %v, want owner/repo-1", sp.Inputs["work_session"])
	}
	if sp.Inputs["judge_ids"] != "ac-met" {
		t.Errorf("judge_ids input = %v, want ac-met", sp.Inputs["judge_ids"])
	}
	if sp.Inputs["resource"] == "" {
		t.Errorf("resource input is empty; want the work resource")
	}
}

// A chain's inputs are checked against the spawned workflow's own contract
// at load, against the parsed declaration, rather than at fire time: the
// target and its contract are both declarations, so nothing has to run for
// the mismatch to be known.
func TestLoadTaskDeclarations_ChainInputsMustSatisfyTheTargetContract(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" }, { check = "resource.state.checks_status", in = ["SUCCESS"] } ]
[chains.inputs]
revision = { from = "resource.state.revision" }
`)},
		[]nodeFixture{{id: "work"}})
	// codex declares a closed inputs contract, so the wired `revision` is not an
	// accepted input.
	writeWorkflowFile(t, cfg, "codex", `
[codex.inputs_schema]
type = "object"
additionalProperties = false
`)
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	_, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err == nil || !strings.Contains(err.Error(), `declares no input "revision"`) {
		t.Fatalf("err = %v, want the target workflow's contract to reject the wired input at load", err)
	}
}

func TestCheckSession_ChainBlockedWhenUnmet(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" }, { check = "resource.state.checks_status", in = ["SUCCESS"] } ]
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	// checks_status FAILURE → when unmet even though judge is pending.
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "FAILURE", "revision": "sha1"})

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if sp.Fired || sp.BlockedReason != chainBlockedWhenUnmet {
		t.Fatalf("expected when_unmet, got %+v", sp)
	}
}

func TestCheckSession_ChainBlockedOutputsMissing(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" }, { check = "resource.state.checks_status", in = ["SUCCESS"] } ]
[chains.inputs]
revision = { from = "resource.state.revision" }
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	// when holds, but the wired `revision` output is absent → not fired.
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS"})

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if sp.Fired {
		t.Fatalf("expected blocked, got fired: %+v", sp)
	}
	if sp.BlockedReason != chainBlockedOutputsMissing || len(sp.MissingOutputs) != 1 || sp.MissingOutputs[0] != "resource.state.revision" {
		t.Fatalf("expected outputs_missing[resource.state.revision], got %+v", sp)
	}
}

func TestCheckSession_ChainChildPlacementParent(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id        = "review"
workflow  = "codex"
placement = "child"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if !sp.Fired || sp.Placement != config.ChainPlacementChild || sp.ParentSession != "owner/repo-1" {
		t.Fatalf("expected child of work session, got %+v", sp)
	}
}

// A sibling chain on a parentless work session spawns under that session's
// own implicit root (domain.ImplicitRootParent) rather than being left
// unreachable — the reviewer is opted into the work session's sibling group,
// not its parent's (there is none).
func TestCheckSession_ChainSiblingWithoutParentUsesImplicitRoot(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	// No parent set on the work session.
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if sp.ParentSession != "root:owner/repo-1" {
		t.Fatalf("ParentSession = %q, want %q", sp.ParentSession, "root:owner/repo-1")
	}
}

// plect tick spawns a fired, not-already-active chain; a chain whose target
// session already exists is reported already-active rather than re-spawned.
func TestTickSession_ChainAlreadyActiveSkips(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
[chains.inputs]
revision = { from = "resource.state.revision" }
judge_ids = { from = "task.done_when.pending_judge_ids" }
pr_url = { from = "resource.state.pr_url" }
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1", "pr_url": "https://github.com/owner/repo/pull/9"})
	// The reviewer session already exists → fire is idempotent (no spawn).
	seedSessionWithNodes(t, store, "owner/repo-1+review-work", "owner/repo", 1, "codex", nil)

	res, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if !sp.Fired || !sp.AlreadyActive || sp.Spawned {
		t.Fatalf("expected already-active without spawn, got %+v", sp)
	}
	if !sp.KickDelivered || sp.KickDebounced {
		t.Fatalf("expected already-active kick delivery, got %+v", sp)
	}
	evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-1+review-work", 0, event.Filter{
		Types: []string{event.TypeUserEmit},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("user.emit count = %d, want 1: %+v", len(evs), evs)
	}
	if evs[0].Source != event.SourceTick || evs[0].Metadata["revision"] != "sha1" || evs[0].Metadata["judge_ids"] != "ac-met" {
		t.Fatalf("unexpected kick event: %+v", evs[0])
	}

	res2, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	sp2, _ := findSpawn(res2.Chains, "review")
	if !sp2.Fired || !sp2.AlreadyActive || sp2.Spawned || !sp2.KickDebounced {
		t.Fatalf("expected already-active debounced on same revision, got %+v", sp2)
	}
	evs, _, _, err = eventlog.NewStore(store.Dir()).List("owner/repo-1+review-work", 0, event.Filter{
		Types: []string{event.TypeUserEmit},
	})
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("same revision should not emit a duplicate kick, got %d events: %+v", len(evs), evs)
	}
}

func TestTickSession_ChainCannotAdoptPopulationOwnedTarget(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	seedSessionWithNodes(t, store, "owner/repo-1+review-work", "owner/repo", 1, "codex", nil)
	if err := store.UpdatePopulation("codex/standing", func(population *state.PopulationState) error {
		population.Workflow = "codex"
		population.Name = "standing"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("owner/repo-1+review-work", func(session *domain.Session) error {
		session.Population = &contract.PopulationProvenance{Workflow: "codex", Name: "standing"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	res, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if !sp.Fired || sp.AlreadyActive || sp.Spawned || len(sp.Warnings) != 1 || !strings.Contains(sp.Warnings[0], "population") {
		t.Fatalf("chain collision = %+v, want a population ownership warning without adoption", sp)
	}
}

// A chain's spawn (Up) failing must not discard the done_when actions this
// same tick already published/persisted, nor abort the whole tick result —
// only that chain's own entry reports the failure, so the next tick can
// retry the (idempotent) fire. The resolved workflow itself must exist here
// (an undefined workflow is caught earlier, at evalChain, as
// workflow_unresolved — see TestTickSession_ChainWorkflowUnresolvedBlocksFire
// below) — this exercises a spawn failure for an unrelated reason, so the
// resource_allowlist boundary is set to deny the resource.
func TestTickSession_ChainSpawnFailureIsNonFatal(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	cfg.ResourceAllowlist = []string{"^$"} // matches nothing: Create() rejects every resource
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	res, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	if len(res.Actions) == 0 {
		t.Fatalf("expected done_when actions to still be reported despite the chain spawn failure, got %+v", res.Actions)
	}
	sp, ok := findSpawn(res.Chains, "review")
	if !ok || !sp.Fired || sp.Spawned {
		t.Fatalf("expected fired-but-not-spawned, got %+v", sp)
	}
	if len(sp.Warnings) == 0 || !strings.Contains(strings.Join(sp.Warnings, " "), "spawn failed") {
		t.Fatalf("expected a spawn-failed warning, got %+v", sp.Warnings)
	}
}

// A chain naming a workflow nothing defines is rejected at load, against the
// declaration, rather than reported as a blocked fire every tick: the target
// is a static reference, so its resolution needs nothing to run.
func TestLoadTaskDeclarations_ChainWorkflowMustResolve(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "missing-workflow"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`)},
		[]nodeFixture{{id: "work"}})
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	_, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", SkipRefresh: true})
	if err == nil || !strings.Contains(err.Error(), "missing-workflow") {
		t.Fatalf("err = %v, want the unresolved workflow named at load", err)
	}
}

func TestCheckSession_NoChainsIsEmpty(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{BaseDir: t.TempDir(), WorkspaceDirsRoot: t.TempDir()}
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	if len(res.Chains) != 0 {
		t.Fatalf("expected no chains, got %+v", res.Chains)
	}
}

// A satisfied judge leaf (approve at the current revision) leaves judge_pending
// false, so the review chain does not fire — the gate is closed only by a real
// independent verdict.
func TestCheckSession_ChainSatisfiedJudgeDoesNotFire(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	seedSessionWithNodes(t, store, "owner/repo-orch", "owner/repo", 1, "", nil)
	seedSessionWithNodes(t, store, "owner/repo-1+review-work", "owner/repo", 1, "codex", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orch")
	setParent(t, store, "owner/repo-1+review-work", "owner/repo-orch")
	// Record an approving sibling verdict at the current revision.
	if _, err := RecordJudge(cfg, store, JudgeParams{
		SessionName:  "owner/repo-1",
		Instance:     "work",
		LeafID:       "ac-met",
		Action:       "approve",
		Reason:       "verified",
		Revision:     "sha1",
		JudgeSession: "owner/repo-1+review-work",
	}); err != nil {
		t.Fatalf("RecordJudge: %v", err)
	}

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if sp.Fired {
		t.Fatalf("expected not fired (judge satisfied), got %+v", sp)
	}
}

// AC1: a task-declared chain is evaluated only against instances of the task
// that declared it. A different task instance sharing the same judge id is
// not a candidate at all — not merely blocked, absent from the plan.
func TestCheckSession_ChainTaskScopedEvaluation(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{
			{
				id:    "a",
				scope: "session",
				setup: "echo '{}'",
				extra: `
[done_when]
all = [ { judge = "ac met", id = "ac-met" } ]

[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`,
			},
			{
				id:    "b",
				scope: "session",
				setup: "echo '{}'",
				extra: `
[done_when]
all = [ { judge = "ac met", id = "ac-met" } ]
`,
			},
		},
		[]nodeFixture{{id: "a"}, {id: "b"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "wf", map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, TaskID: "a", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
		"b": {Scope: contract.TaskScopeSession, TaskID: "b", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	res, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err != nil {
		t.Fatalf("CheckSession: %v", err)
	}
	var aEntries, bEntries int
	for _, sp := range res.Chains {
		switch sp.Instance {
		case "a":
			aEntries++
			if sp.Task != "a" || !sp.Fired {
				t.Errorf("instance a spawn = %+v, want fired for task a", sp)
			}
		case "b":
			bEntries++
		}
	}
	if aEntries != 1 {
		t.Fatalf("expected exactly 1 evaluation for instance a, got %d", aEntries)
	}
	if bEntries != 0 {
		t.Fatalf("expected task b instance to have no chain candidates at all, got %d entries", bEntries)
	}
}

// AC5 (dual-read retired): a legacy chains/*.toml file sitting under the
// config base dir contributes nothing to the evaluated plan — it is not
// merged, never fires. It is still surfaced as a migration-nudge warning
// (config.LegacyChainsDirNotice) so a straggler file isn't silently inert
// with zero signal.
func TestCheckSession_LegacyChainsDirIsRefused(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{
			{id: "a", scope: "session", setup: "echo '{}'", extra: "[done_when]\nall = [ { judge = \"ac met\", id = \"ac-met\" } ]\n"},
		},
		[]nodeFixture{{id: "a"}})
	legacyDir := filepath.Join(cfg.BaseDir, "chains")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "review.toml"), []byte(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "wf", map[string]*contract.TaskState{
		"a": {Scope: contract.TaskScopeSession, TaskID: "a", Status: contract.TaskStatusProduced, Outputs: map[string]any{}},
	})

	_, err := CheckSession(cfg, store, CheckParams{SessionName: "owner/repo-1"})
	if err == nil || !strings.Contains(err.Error(), "review.toml") {
		t.Fatalf("err = %v, want the load to refuse the leftover chains/*.toml by name", err)
	}
}

// A chain can hand its reviewer a pull request in a different repository
// than the session's own resource: the fact is recorded into the instance,
// and the projection reads it from there.
func TestTickSession_ChainFiresOnExplicitPRURLAcrossRepos(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{
			id: "work",
			extra: `
[done_when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge = "ac met", id = "ac-met" },
]

[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
[chains.inputs]
revision = { from = "resource.state.revision" }
pr_url   = { from = "self.state.pr_url" }
`,
		}},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")

	const crossRepoPRURL = "https://github.com/owner/repo-2/pull/42"
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "wf", map[string]*contract.TaskState{
		"work": {
			Scope:  contract.TaskScopeSession,
			TaskID: "work",
			Status: contract.TaskStatusProduced,
			// Tracked by an issue in a different repository than the PR.
			Resource: "https://github.com/owner/repo/issues/1",
			Observed: &contract.ResourceObservation{
				State: map[string]any{"revision": "sha1", "checks_status": "SUCCESS"},
				At:    time.Now(),
			},
			State: map[string]any{"pr_url": crossRepoPRURL},
		},
	})

	res, err := TickSession(cfg, store, TickParams{SessionName: "owner/repo-1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if !sp.Fired {
		t.Fatalf("expected the review chain to fire, got %+v", sp)
	}
	if sp.Inputs["pr_url"] != crossRepoPRURL {
		t.Fatalf("pr_url input = %v, want %q", sp.Inputs["pr_url"], crossRepoPRURL)
	}
}
