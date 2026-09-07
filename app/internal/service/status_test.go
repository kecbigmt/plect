package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// Status's "work" layer must carry the same decision-making material the
// retired `plect check` used to report per instance: the done_when evaluation,
// the heartbeat budget, the classified action, and the chain plan for that same
// instance.
func TestStatus_WorkCarriesActionHeartbeatBudgetAndChains(t *testing.T) {
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
revision = "{{.Work.outputs.revision}}"
`)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	result, err := Status(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(result.Work) != 1 {
		t.Fatalf("len(Work) = %d, want 1", len(result.Work))
	}
	w := result.Work[0]
	if w.Instance != "work" {
		t.Fatalf("Instance = %q, want work", w.Instance)
	}
	if w.DoneWhen == nil || w.DoneWhen.Overall != task.DonePending {
		t.Fatalf("DoneWhen.Overall = %+v, want pending (checks_status satisfied, judge not yet recorded)", w.DoneWhen)
	}
	if w.Action != "review_required" {
		t.Errorf("Action = %q, want review_required", w.Action)
	}
	if w.ReviewerCommand == "" {
		t.Error("expected a non-empty ReviewerCommand for review_required")
	}
	if w.HeartbeatBudget == 0 {
		t.Error("expected heartbeat budget to be reported")
	}
	if len(w.Chains) != 1 || w.Chains[0].ChainID != "review" {
		t.Fatalf("Chains = %+v, want one entry for chain \"review\"", w.Chains)
	}
	if !w.Chains[0].Fired {
		t.Errorf("expected chain \"review\" to be fired, got %+v", w.Chains[0])
	}
	if w.Observed == nil || w.Observed.State["checks_status"] != "SUCCESS" {
		t.Errorf("observed checks_status = %+v, want SUCCESS", w.Observed)
	}
}

func TestStatus_RuntimeCarriesHealthMovementTimestamps(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "true")
	lastCheckedAt := time.Now().Add(-time.Minute).UTC()
	lastMovementAt := time.Now().Add(-2 * time.Minute).UTC()
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})
	if err := store.Update("owner/repo-1", func(s *domain.Session) error {
		s.Health = &contract.HealthState{LastCheckedAt: lastCheckedAt, LastActivityAt: lastMovementAt}
		return nil
	}); err != nil {
		t.Fatalf("seed health state: %v", err)
	}

	result, err := Status(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if result.Runtime.LastCheckedAt.IsZero() || result.Runtime.LastActivityAt.IsZero() {
		t.Fatalf("runtime = %+v, want health timestamps", result.Runtime)
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), "last_activity_at") || strings.Contains(string(b), "last_progress_at") {
		t.Fatalf("status JSON = %s, want last_activity_at only", b)
	}
}

// TestStatus_SurfacesActivityProbeFaultsAsWarnings pins the user-facing half
// of the probe-fault report: a probe that cannot produce an envelope
// contributes nothing to health, so without a warning it would be
// indistinguishable from a session that is simply quiet.
func TestStatus_SurfacesActivityProbeFaultsAsWarnings(t *testing.T) {
	store := testStore(t)
	cfg := activityFixtureConfig(t, "echo 'pane is gone' >&2; exit 3")
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	})

	result, err := Status(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "initial") && strings.Contains(w, "pane is gone") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Warnings = %q, want one naming the failing activity probe", result.Warnings)
	}
}

// StatusIdentity.Inputs projects the create-time workflow inputs already
// persisted on the session (contract.Session.Inputs) — a session-identity
// fact that predates any run, not something Runtime/Work recomputes.
func TestStatus_IdentityCarriesCreateTimeInputs(t *testing.T) {
	store := testStore(t)
	cfg := aliveFixtureConfig(t, "true")
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default", nil)
	if err := store.Update("owner/repo-1", func(session *domain.Session) error {
		session.Inputs = map[string]any{"reviewer": "alice", "retries": float64(3)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := Status(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got := result.Identity.Inputs["reviewer"]; got != "alice" {
		t.Errorf("Identity.Inputs[\"reviewer\"] = %v, want alice", got)
	}
	if got := result.Identity.Inputs["retries"]; got != float64(3) {
		t.Errorf("Identity.Inputs[\"retries\"] = %v, want 3", got)
	}
}

func TestStatus_SlackThreadNodeOutputsAreReadableUnderTheNode(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "default",
		[]taskFixture{{id: "slack_thread", scope: contract.TaskScopeSession, setup: "true"}},
		[]nodeFixture{{id: "slack_thread"}})
	seedSessionWithNodes(t, store, "owner/repo-1", "owner/repo", 1, "default", map[string]*contract.TaskState{
		"slack_thread": {
			Scope:  contract.TaskScopeSession,
			TaskID: "slack_thread",
			Status: contract.TaskStatusProduced,
			Outputs: map[string]any{
				"thread_ts":  "1234567890.123456",
				"channel_id": "C01ABCDEF",
				"permalink":  "https://example.test/archives/C01ABCDEF/p1234567890123456",
			},
		},
	})

	result, err := Status(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	var work *StatusTask
	for i := range result.Work {
		if result.Work[i].Instance == "slack_thread" {
			work = &result.Work[i]
		}
	}
	if work == nil {
		t.Fatalf("Work = %+v, want an entry for slack_thread", result.Work)
	}
	if work.Outputs["thread_ts"] != "1234567890.123456" || work.Outputs["channel_id"] != "C01ABCDEF" ||
		work.Outputs["permalink"] != "https://example.test/archives/C01ABCDEF/p1234567890123456" {
		t.Errorf("slack_thread outputs = %+v, want thread_ts/channel_id/permalink readable", work.Outputs)
	}
}

// Summarize is the default `plect status --json` projection: only instances
// with a done_when, and only the leaf/chain fields an orchestrator needs —
// never the instance's full (unfiltered) outputs map.
func TestSummarize_FiltersToDoneWhenInstancesAndOmitsRawOutputs(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{
			workTaskWithChain(`
[[chains]]
id       = "review"
workflow = "codex"
[chains.when]
all = [
  { judge_pending = "ac-met" },
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
]
`),
			{id: "envfile", scope: "run", setup: "echo '{}'"},
		},
		[]nodeFixture{{id: "work"}, {id: "envfile"}})
	writeWorkflowFile(t, cfg, "codex", "")
	seedReviewWork(t, store, "owner/repo-1", map[string]any{
		"checks_status": "SUCCESS",
		"revision":      "sha1",
		"instruction":   "a very long unreferenced instruction blob",
	})
	if err := store.UpdatePopulation("wf/dispatch", func(population *state.PopulationState) error {
		population.Workflow = "wf"
		population.Name = "dispatch"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update("owner/repo-1", func(session *domain.Session) error {
		session.Population = &contract.PopulationProvenance{Workflow: "wf", Name: "dispatch"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := Status(cfg, store, "owner/repo-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	sum := Summarize(result)

	if sum.Identity.SessionName != "owner/repo-1" {
		t.Errorf("Identity.SessionName = %q", sum.Identity.SessionName)
	}
	if sum.Identity.Population == nil || sum.Identity.Population.Name != "dispatch" {
		t.Fatalf("Identity.Population = %+v, want dispatch provenance", sum.Identity.Population)
	}
	if len(sum.Work) != 1 {
		t.Fatalf("len(Work) = %d, want 1 (only the done_when-bearing instance)", len(sum.Work))
	}
	w := sum.Work[0]
	if w.Instance != "work" {
		t.Errorf("Instance = %q, want work", w.Instance)
	}
	if w.DoneWhen == nil || len(w.DoneWhen.Leaves) != 2 {
		t.Fatalf("DoneWhen = %+v, want 2 leaves", w.DoneWhen)
	}
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "instruction") {
		t.Errorf("summary JSON leaked an output not referenced by done_when: %s", b)
	}
	if len(w.Chains) != 1 || w.Chains[0].ChainID != "review" {
		t.Fatalf("Chains = %+v, want one entry for chain \"review\"", w.Chains)
	}
}

// TestTombstoneStatusResult_ReportsNodesAndTasksSeparately pins a tombstone
// migrated by docs/migrations/tombstone-nodes-tasks-migration.md's procedure
// (a real tombstone.json blob with the split "nodes"/"tasks" keys that
// procedure produces): the node entries (@workflow, tmux) must report
// dynamic=false and the dynamic instance (review#1) dynamic=true, the same
// distinction the pre-split flat map's per-entry field used to carry.
func TestTombstoneStatusResult_ReportsNodesAndTasksSeparately(t *testing.T) {
	const migratedTombstoneJSON = `{
		"session_name": "org/repo-1",
		"resource_id": "https://github.com/org/repo/issues/1",
		"workflow": "default",
		"nodes": {
			"@workflow": {"scope": "session", "status": "cleaned", "outputs": {"branch": "issue/1"}},
			"tmux": {"scope": "run", "status": "cleaned", "outputs": {}}
		},
		"tasks": {
			"review#1": {"scope": "session", "status": "produced", "task_id": "review", "resource": "pr-1", "outputs": {"checks_status": "SUCCESS"}}
		},
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-02T00:00:00Z",
		"destroyed_at": "2026-01-03T00:00:00Z"
	}`
	var tomb contract.Tombstone
	if err := json.Unmarshal([]byte(migratedTombstoneJSON), &tomb); err != nil {
		t.Fatalf("unmarshal migrated tombstone: %v", err)
	}
	if len(tomb.Nodes) != 2 || len(tomb.Tasks) != 1 {
		t.Fatalf("Nodes/Tasks = %d/%d, want 2/1", len(tomb.Nodes), len(tomb.Tasks))
	}

	result := tombstoneStatusResult(&tomb)
	byInstance := make(map[string]StatusTask, len(result.Work))
	for _, w := range result.Work {
		byInstance[w.Instance] = w
	}
	if got := byInstance["tmux"]; got.IsTask {
		t.Errorf("tmux.IsTask = %v, want false (a workflow node)", got.IsTask)
	}
	if got := byInstance["review#1"]; !got.IsTask {
		t.Errorf("review#1.IsTask = %v, want true (a dynamic instance)", got.IsTask)
	}
	if got := byInstance["review#1"]; got.Resource != "pr-1" {
		t.Errorf("review#1.Resource = %q, want pr-1", got.Resource)
	}
}

// TestTombstoneStatusResult_TasksOnlyShapeIsNotMistakenForLegacy pins the
// Go-side read of a tombstone with only dynamic instances and no "nodes" key
// at all — omitempty drops Nodes when a session-scoped task document was set
// up before any workflow node ever ran. It must read as entirely dynamic.
func TestTombstoneStatusResult_TasksOnlyShapeIsNotMistakenForLegacy(t *testing.T) {
	const tasksOnlyTombstoneJSON = `{
		"session_name": "org/repo-2",
		"tasks": {
			"review#1": {"scope": "session", "status": "produced", "task_id": "review", "resource": "pr-2", "outputs": {}}
		},
		"created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-02T00:00:00Z",
		"destroyed_at": "2026-01-03T00:00:00Z"
	}`
	var tomb contract.Tombstone
	if err := json.Unmarshal([]byte(tasksOnlyTombstoneJSON), &tomb); err != nil {
		t.Fatalf("unmarshal tasks-only tombstone: %v", err)
	}
	if len(tomb.Nodes) != 0 || len(tomb.Tasks) != 1 {
		t.Fatalf("Nodes/Tasks = %d/%d, want 0/1", len(tomb.Nodes), len(tomb.Tasks))
	}

	result := tombstoneStatusResult(&tomb)
	if len(result.Work) != 1 || !result.Work[0].IsTask {
		t.Fatalf("Work = %+v, want one dynamic instance", result.Work)
	}
}
