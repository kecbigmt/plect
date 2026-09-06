package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// capReviewChainFixture is workTaskWithChain's "review" chain with no
// resource/inputs projections — the cap refusal below happens before any of
// those would matter, since reserveChildCapSlot runs inside Up, ahead of
// Create.
const capReviewChainFixture = `
[[chains]]
id       = "review"
workflow = "reviewer_wf"
[chains.when]
all = [ { judge_pending = "ac-met" } ]
`

// writeSpawnableWorkflowFile is writeWorkflowFile's real-spawn counterpart:
// its provider actually creates the reviewer's workspace directory and
// reports it under the key Up expects (rather than writeWorkflowFile's fixed,
// differently-keyed echoed outputs), and it declares the one node a workflow
// needs to be buildable at all — needed only by the test below that must
// observe a spawn actually succeed once the cap frees, not just a derived
// target name.
func writeSpawnableWorkflowFile(t *testing.T, cfg *config.Config, id, workspaceDir string) {
	t.Helper()
	tasksDir := filepath.Join(cfg.BaseDir, "tasks")
	workflowsDir := filepath.Join(cfg.BaseDir, "workflows")
	providersDir := filepath.Join(cfg.BaseDir, "workspaces")
	for _, dir := range []string{tasksDir, workflowsDir, providersDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	noopID := id + "_noop"
	noopDoc := effectFixtureDoc(taskFixture{id: noopID, scope: "session", setup: "echo '{}'"})
	if err := os.WriteFile(filepath.Join(tasksDir, noopID+".toml"), []byte(noopDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	provID, prov := providerAside(id, providerCreatingWorkspace(id, workspaceDir))
	wf := fmt.Sprintf("[%s]\nkind = \"workflow\"\nworkspace_provider = %q\n\n[[%s.nodes]]\nuses = %q\n", id, provID, id, noopID)
	if err := os.WriteFile(filepath.Join(workflowsDir, id+".toml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providersDir, provID+".toml"), []byte(prov), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTickSession_ChainCapRefusalReportsTypedOutcomeAndEmitsChainAttemptEvent(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(capReviewChainFixture)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "reviewer_wf", "")
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))

	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "sibling", "acct", 2, "", upTasks())
	setParent(t, store, "sibling", "parent1")
	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	setParent(t, store, "work1", "parent1")

	res, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	sp, ok := findSpawn(res.Chains, "review")
	if !ok || !sp.Fired || sp.Spawned || sp.AlreadyActive || !sp.CapRefused {
		t.Fatalf("expected a cap-refused typed outcome, got %+v", sp)
	}
	if sp.TargetSession == "" {
		t.Fatalf("expected a derived target session even though the spawn was refused, got %+v", sp)
	}
	if store.Get(sp.TargetSession) != nil {
		t.Fatalf("cap refusal must create no target session, found %q", sp.TargetSession)
	}
	if len(sp.Warnings) == 0 {
		t.Fatalf("expected the cap error surfaced as a warning, got none")
	}

	evs, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("chain-attempt events = %d, want 1: %+v", len(evs), evs)
	}
	if got := evs[0].Metadata; got["chain_id"] != "review" || got["instance"] != "work" || got["target"] != sp.TargetSession || got["reason"] != "cap" {
		t.Fatalf("chain-attempt metadata = %+v, want chain_id=review instance=work target=%s reason=cap", got, sp.TargetSession)
	}
	if evs[0].Source != event.SourceTick {
		t.Fatalf("chain-attempt source = %q, want %q", evs[0].Source, event.SourceTick)
	}

	// A second refusal streak on unchanged state records nothing further.
	if _, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true}); err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	evs2, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(evs2) != 1 {
		t.Fatalf("chain-attempt events after a second refusal = %d, want still 1 (deduped)", len(evs2))
	}
}

func TestTickSession_ChainSpawnsOnceCapacityFreesAfterCapRefusal(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(capReviewChainFixture)},
		[]nodeFixture{{id: "work"}})
	writeSpawnableWorkflowFile(t, cfg, "reviewer_wf", filepath.Join(t.TempDir(), "reviewer-wd"))
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))

	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "sibling", "acct", 2, "", upTasks())
	setParent(t, store, "sibling", "parent1")
	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	setParent(t, store, "work1", "parent1")

	res, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession: %v", err)
	}
	sp, _ := findSpawn(res.Chains, "review")
	if !sp.CapRefused {
		t.Fatalf("expected the first tick to be cap-refused, got %+v", sp)
	}

	// The sibling frees its slot.
	if err := store.Update("sibling", func(s *domain.Session) error {
		s.Tasks["run_node"].Status = contract.TaskStatusCleaned
		return nil
	}); err != nil {
		t.Fatalf("bring sibling down: %v", err)
	}

	res2, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	sp2, ok := findSpawn(res2.Chains, "review")
	if !ok || !sp2.Fired || !sp2.Spawned || sp2.CapRefused {
		t.Fatalf("expected the retried fire to spawn once capacity freed, got %+v", sp2)
	}
	if store.Get(sp2.TargetSession) == nil {
		t.Fatalf("spawned target %q not persisted", sp2.TargetSession)
	}
}

func TestTickSession_ChainCapAttemptEventRecordsNewStreakAfterSpawnAndDestroy(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(capReviewChainFixture)},
		[]nodeFixture{{id: "work"}})
	writeSpawnableWorkflowFile(t, cfg, "reviewer_wf", filepath.Join(t.TempDir(), "reviewer-wd"))
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))

	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "sibling", "acct", 2, "", upTasks())
	setParent(t, store, "sibling", "parent1")
	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	setParent(t, store, "work1", "parent1")

	if _, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true}); err != nil {
		t.Fatalf("TickSession(1): %v", err)
	}
	evs, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List(1): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("chain-attempt events after the first refusal = %d, want 1: %+v", len(evs), evs)
	}

	if err := store.Update("sibling", func(s *domain.Session) error {
		s.Tasks["run_node"].Status = contract.TaskStatusCleaned
		return nil
	}); err != nil {
		t.Fatalf("bring sibling down: %v", err)
	}
	res, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	sp, ok := findSpawn(res.Chains, "review")
	if !ok || !sp.Spawned {
		t.Fatalf("expected the retried fire to spawn once capacity freed, got %+v", sp)
	}
	target := sp.TargetSession

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: target, Force: true}); err != nil {
		t.Fatalf("Destroy(target): %v", err)
	}

	if err := store.Update("sibling", func(s *domain.Session) error {
		s.Tasks["run_node"].Status = contract.TaskStatusProduced
		return nil
	}); err != nil {
		t.Fatalf("bring sibling back up: %v", err)
	}
	res2, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(3): %v", err)
	}
	sp2, ok := findSpawn(res2.Chains, "review")
	if !ok || !sp2.CapRefused || sp2.TargetSession != target {
		t.Fatalf("expected a second cap refusal for the same target %q, got %+v", target, sp2)
	}
	evs2, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(evs2) != 2 {
		t.Fatalf("chain-attempt events across two refusal streaks = %d, want 2 (one per streak): %+v", len(evs2), evs2)
	}
}

func TestTickSession_ChainCapAttemptEventRecordsNewStreakAfterPredicateGoesUnmetAndTrueAgain(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(capReviewChainFixture)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "reviewer_wf", "")
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))

	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "sibling", "acct", 2, "", upTasks())
	setParent(t, store, "sibling", "parent1")
	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	setParent(t, store, "work1", "parent1")

	if _, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true}); err != nil {
		t.Fatalf("TickSession(1): %v", err)
	}
	evs, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List(1): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("chain-attempt events after the first refusal = %d, want 1: %+v", len(evs), evs)
	}

	if err := store.Update("work1", func(s *domain.Session) error {
		st := s.Tasks["work"]
		st.DoneWhen.Judges = map[string]*contract.DoneWhenJudge{
			"ac-met": {
				LeafID:          "ac-met",
				Action:          task.JudgeActionApprove,
				Revision:        "sha1",
				ReviewerSession: "some-reviewer",
				Relation:        string(domain.RelationSibling),
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("record a satisfying verdict at the current revision: %v", err)
	}

	res, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	sp, ok := findSpawn(res.Chains, "review")
	if !ok || sp.Fired {
		t.Fatalf("expected the chain to stop firing once the judge leaf is satisfied, got %+v", sp)
	}

	if err := store.Update("work1", func(s *domain.Session) error {
		s.Tasks["work"].Observed.State["revision"] = "sha2"
		return nil
	}); err != nil {
		t.Fatalf("advance the observed revision past the recorded verdict: %v", err)
	}

	res2, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(3): %v", err)
	}
	sp2, ok := findSpawn(res2.Chains, "review")
	if !ok || !sp2.CapRefused {
		t.Fatalf("expected the chain to refire and be cap-refused again, got %+v", sp2)
	}
	evs2, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(evs2) != 2 {
		t.Fatalf("chain-attempt events across two refusal streaks separated by a satisfied judge = %d, want 2 (one per streak): %+v", len(evs2), evs2)
	}
}

// blockLogFile makes every eventlog.Append against store fail
// deterministically: each call site builds a fresh eventlog.Store over
// store.Dir() (service/event.go's own convention), so stripping write
// permission from the shared store.db forces that fresh open to fail —
// while store's own state.Store connection, already established before
// this runs, keeps working, since Unix permission checks apply at open(2),
// not to an already-open descriptor. It returns the blocked path so the
// caller can restore it once the test no longer needs the failure.
func blockLogFile(t *testing.T, store *state.Store) string {
	t.Helper()
	dbPath := filepath.Join(store.Dir(), "store.db")
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func TestTickSession_ChainCapAttemptEventRetriesAfterPublishFailure(t *testing.T) {
	store := testStore(t)
	// No [done_when]: this instance's own done_when action would also try to
	// publish to the blocked log below and fail TickSession outright before
	// the chain loop this test targets ever runs. The chain's own `when`
	// reads resource.state directly, so it needs no done_when leaf to fire.
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{{id: "work", extra: `
[[chains]]
id       = "review"
workflow = "reviewer_wf"
[chains.when]
all = [ { check = "resource.state.checks_status", in = ["SUCCESS"] } ]
`}},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "reviewer_wf", "")
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))

	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "sibling", "acct", 2, "", upTasks())
	setParent(t, store, "sibling", "parent1")
	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS"})
	setParent(t, store, "work1", "parent1")

	dbPath := blockLogFile(t, store)

	res, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(1): %v", err)
	}
	sp, ok := findSpawn(res.Chains, "review")
	if !ok || !sp.CapRefused {
		t.Fatalf("expected a cap-refused outcome despite the blocked log, got %+v", sp)
	}
	failed := false
	for _, w := range sp.Warnings {
		if strings.Contains(w, "chain-attempt event failed") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("expected a chain-attempt event failure warning, got %+v", sp.Warnings)
	}

	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true})
	if err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	sp2, ok := findSpawn(res2.Chains, "review")
	if !ok || !sp2.CapRefused {
		t.Fatalf("expected a cap-refused outcome, got %+v", sp2)
	}
	evs, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("chain-attempt events once the log unblocked = %d, want 1 (the retried publish, not permanently dropped)", len(evs))
	}
}

func TestTickSession_ChainCapAttemptMarkerDoesNotSurviveRecreationUnderTheSameName(t *testing.T) {
	store := testStore(t)
	// This test's own Destroy call compiles "wf", unlike its sibling tests
	// (which only ever tick/check it): the node list needs a real effect, so
	// "work" — a task/gate document, never a workflow DAG node, seeded
	// straight into state below — stays off it.
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{
			workTaskWithChain(capReviewChainFixture),
			{id: "noop_node", scope: "session", setup: "echo '{}'"},
		},
		[]nodeFixture{{id: "noop_node"}})
	writeWorkflowFile(t, cfg, "reviewer_wf", "")
	writeCapWorkflow(t, cfg.BaseDir, "parent_wf", intPtr(1))

	seedSession(t, store, "parent1", "acct", 1, "parent_wf", nil)
	seedSession(t, store, "sibling", "acct", 2, "", upTasks())
	setParent(t, store, "sibling", "parent1")
	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	setParent(t, store, "work1", "parent1")

	if _, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true}); err != nil {
		t.Fatalf("TickSession(1): %v", err)
	}
	evs, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List(1): %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("chain-attempt events before destroy = %d, want 1", len(evs))
	}

	if _, err := Destroy(cfg, store, DestroyParams{Identifier: "work1", Force: true}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})
	setParent(t, store, "work1", "parent1")

	if _, err := TickSession(cfg, store, TickParams{SessionName: "work1", SkipRefresh: true}); err != nil {
		t.Fatalf("TickSession(2): %v", err)
	}
	evs2, _, _, err := eventlog.NewStore(store.Dir()).List("work1", 0, event.Filter{Types: []string{event.TypeChainAttempt}})
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(evs2) != 2 {
		t.Fatalf("chain-attempt events across a destroy and same-name recreation = %d, want 2 (the recreated session's own first refusal, not suppressed by the destroyed one's marker)", len(evs2))
	}
}

func TestEvaluateSessionActions_ReturnedSnapshotIsImmuneToALaterStoreMutation(t *testing.T) {
	store := testStore(t)
	cfg := writeWorkflowFixture(t, t.TempDir(), "wf",
		[]taskFixture{workTaskWithChain(capReviewChainFixture)},
		[]nodeFixture{{id: "work"}})
	writeWorkflowFile(t, cfg, "reviewer_wf", "")
	seedReviewWork(t, store, "work1", map[string]any{"checks_status": "SUCCESS", "revision": "sha1"})

	_, session, _, _, err := evaluateSessionActions(cfg, store, "work1", false, "")
	if err != nil {
		t.Fatalf("evaluateSessionActions: %v", err)
	}
	snapshot := sessionGeneration(session)

	if err := store.Update("work1", func(s *domain.Session) error {
		s.CreatedAt = s.CreatedAt.Add(time.Hour)
		return nil
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	if got := sessionGeneration(session); got != snapshot {
		t.Fatalf("sessionGeneration(session) = %q after an unrelated store mutation, want %q unchanged", got, snapshot)
	}
	if live := sessionGeneration(store.Get("work1")); live == snapshot {
		t.Fatal("test setup did not actually change the live session's generation")
	}
}
