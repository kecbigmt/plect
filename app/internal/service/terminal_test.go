package service

import (
	"testing"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

func TestPublishTerminalToParent_NoParentIsNoOp(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", nil)

	id, wakeErr, err := PublishTerminalToParent(cfg, store, "owner/repo-1", TerminalParams{
		Type:    event.TypeTerminalDone,
		Summary: "done",
	})
	if err != nil {
		t.Fatalf("PublishTerminalToParent: %v", err)
	}
	if wakeErr != nil {
		t.Fatalf("wakeErr = %v, want nil", wakeErr)
	}
	if id != "" {
		t.Fatalf("id = %q, want empty (no parent to push to)", id)
	}
}

func TestPublishTerminalToParent_RootPrefixDeliversToRootTarget(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-reviewer", "owner/repo", 1, "claude", nil)
	setParent(t, store, "owner/repo-reviewer", "root:owner/repo-1")

	id, _, err := PublishTerminalToParent(cfg, store, "owner/repo-reviewer", TerminalParams{
		Type:    event.TypeTerminalDone,
		Summary: "goal-met approved",
	})
	if err != nil {
		t.Fatalf("PublishTerminalToParent: %v", err)
	}
	if id == "" {
		t.Fatal("expected a stored event id")
	}

	targetEvs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-1", 0, event.Filter{})
	if err != nil {
		t.Fatalf("list target log: %v", err)
	}
	if len(targetEvs) != 1 {
		t.Fatalf("target log events = %d, want 1", len(targetEvs))
	}
	if rel := targetEvs[0].Metadata[event.MetaRelation]; rel != string(domain.RelationSibling) {
		t.Errorf("relation = %q, want %q", rel, domain.RelationSibling)
	}
}

func TestPublishTerminalToParent_WritesIntoParentsOwnLog(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "claude", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")

	// The parent has no run-scoped task, so a best-effort wake is attempted and
	// fails (no workflow configured) — irrelevant to this test's concern (the
	// event lands in the parent's own log), so wakeErr is deliberately ignored.
	id, _, err := PublishTerminalToParent(cfg, store, "owner/repo-1", TerminalParams{
		Type:     event.TypeTerminalDone,
		Summary:  "work done",
		Metadata: map[string]string{event.MetaInstance: "initial"},
		DedupKey: "initial|done|fp1",
	})
	if err != nil {
		t.Fatalf("PublishTerminalToParent: %v", err)
	}
	if id == "" {
		t.Fatal("expected a stored event id")
	}

	// It must land in the PARENT's log (its own dispatcher only ever reads its
	// own log), not the origin's.
	parentEvs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-orchestrator", 0, event.Filter{})
	if err != nil {
		t.Fatalf("list parent log: %v", err)
	}
	if len(parentEvs) != 1 {
		t.Fatalf("parent log events = %d, want 1", len(parentEvs))
	}
	ev := parentEvs[0]
	if ev.Type != event.TypeTerminalDone || ev.DeliveryMode != event.DeliveryModePush {
		t.Fatalf("event = %+v, want push done", ev)
	}
	if want := "work done (from owner/repo-1)"; ev.Summary != want {
		t.Fatalf("summary = %q, want %q (origin must be self-describing on the parent's log)", ev.Summary, want)
	}
	if ev.Metadata[event.MetaOriginSession] != "owner/repo-1" {
		t.Fatalf("origin_session = %q, want owner/repo-1", ev.Metadata[event.MetaOriginSession])
	}
	if ev.Metadata[event.MetaRelation] != string(domain.RelationChild) {
		t.Fatalf("relation (from parent's POV) = %q, want child", ev.Metadata[event.MetaRelation])
	}
	if ev.Metadata[event.MetaInstance] != "initial" {
		t.Fatalf("instance metadata = %q, want initial", ev.Metadata[event.MetaInstance])
	}

	originEvs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-1", 0, event.Filter{})
	if err != nil {
		t.Fatalf("list origin log: %v", err)
	}
	if len(originEvs) != 0 {
		t.Fatalf("origin log events = %d, want 0 (push writes to the receiver, not the origin)", len(originEvs))
	}
}

func TestPublishTerminalToParent_DeadSummaryAlreadySelfDescribingIsNotDoubled(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "claude", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")

	_, _, err := PublishTerminalToParent(cfg, store, "owner/repo-1", TerminalParams{
		Type:    event.TypeTerminalDead,
		Summary: "owner/repo-1 is dead: healthcheck failed",
	})
	if err != nil {
		t.Fatalf("PublishTerminalToParent: %v", err)
	}

	evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-orchestrator", 0, event.Filter{Types: []string{event.TypeTerminalDead}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("dead events on parent = %d, want 1", len(evs))
	}
	if want := "owner/repo-1 is dead: healthcheck failed"; evs[0].Summary != want {
		t.Fatalf("summary = %q, want %q unchanged (already self-describing)", evs[0].Summary, want)
	}
}

func TestPublishTerminalToParent_DedupSkipsRepeatedPush(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "claude", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")

	params := TerminalParams{
		Type:     event.TypeTerminalEscalate,
		Summary:  "stuck",
		DedupKey: "initial|escalate|fpA",
	}
	first, _, err := PublishTerminalToParent(cfg, store, "owner/repo-1", params)
	if err != nil || first == "" {
		t.Fatalf("first push: id=%q err=%v", first, err)
	}
	second, _, err := PublishTerminalToParent(cfg, store, "owner/repo-1", params)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if second != "" {
		t.Fatalf("second push id = %q, want empty (deduped)", second)
	}

	evs, _, _, err := eventlog.NewStore(store.Dir()).List("owner/repo-orchestrator", 0, event.Filter{Types: []string{event.TypeTerminalEscalate}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("escalate events on parent = %d, want 1 (deduped)", len(evs))
	}
}

func TestPublishTerminalToParent_DedupIsPerType(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "claude", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")

	sameKey := "initial|any|fpA"
	if _, _, err := PublishTerminalToParent(cfg, store, "owner/repo-1", TerminalParams{
		Type: event.TypeTerminalEscalate, DedupKey: sameKey,
	}); err != nil {
		t.Fatalf("escalate push: %v", err)
	}
	id, _, err := PublishTerminalToParent(cfg, store, "owner/repo-1", TerminalParams{
		Type: event.TypeTerminalDone, DedupKey: sameKey,
	})
	if err != nil {
		t.Fatalf("done push: %v", err)
	}
	if id == "" {
		t.Fatal("a done push with the same dedup key as an unrelated escalate push must not be suppressed")
	}
}

func TestPublishTerminalToParent_RunScopeUpTargetIsNotWoken(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", map[string]*contract.TaskState{
		"tmux": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
	})
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "claude", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")

	// The parent's run scope is already up: publishTerminalTo must not attempt
	// to bring it up again (Up with no configured workflow would error if
	// called against an already-produced target in a way that mutated state).
	id, wakeErr, err := PublishTerminalToParent(cfg, store, "owner/repo-1", TerminalParams{
		Type: event.TypeTerminalDone, DedupKey: "initial|done|fp1",
	})
	if err != nil || id == "" {
		t.Fatalf("PublishTerminalToParent: id=%q err=%v", id, err)
	}
	if wakeErr != nil {
		t.Fatalf("wakeErr = %v, want nil (target already up, no wake attempted)", wakeErr)
	}
	tasks := store.Get("owner/repo-orchestrator").Tasks
	if len(tasks) != 1 {
		t.Fatalf("parent tasks mutated: %+v, want untouched single tmux task", tasks)
	}
}

func TestPublishTerminalToParent_DownTargetWakeFailureIsReturnedButDoesNotBreakThePush(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	// Parent has no run-scoped task at all (down/never-up). Up will be
	// attempted best-effort and, with no workflow configured, will fail
	// internally — the push itself must still have succeeded and been
	// recorded (err is nil), but the wake failure must now be observable via
	// wakeErr rather than silently discarded.
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "claude", nil)
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")

	id, wakeErr, err := PublishTerminalToParent(cfg, store, "owner/repo-1", TerminalParams{
		Type: event.TypeTerminalDone, DedupKey: "initial|done|fp1",
	})
	if err != nil || id == "" {
		t.Fatalf("PublishTerminalToParent: id=%q err=%v", id, err)
	}
	if wakeErr == nil {
		t.Fatal("wakeErr = nil, want non-nil (Up has no configured workflow for the down target)")
	}

	evs, _, _, lerr := eventlog.NewStore(store.Dir()).List("owner/repo-orchestrator", 0, event.Filter{Types: []string{event.TypeTerminalDone}})
	if lerr != nil {
		t.Fatalf("list parent: %v", lerr)
	}
	if len(evs) != 1 {
		t.Fatalf("done events on parent = %d, want 1 (recorded despite wake failure)", len(evs))
	}
}

// The invariant must hold even if a caller passes wakeIfDown=true by mistake.
func TestPublishTerminalTo_SelfTargetNeverWakesRegardlessOfCallerRequest(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{WorkspaceDirsRoot: t.TempDir()}
	// No workflow configured, so an attempted Up would fail loudly — proving
	// absence of a wake attempt (wakeErr == nil) rather than merely a failed one.
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", nil)

	id, wakeErr, err := publishTerminalTo(cfg, store, "owner/repo-1", "owner/repo-1", true, TerminalParams{
		Type:     event.TypeTerminalDead,
		Summary:  "owner/repo-1 health escalation is undeliverable",
		DedupKey: "owner/repo-1|health|unhealthy|1|undeliverable",
	})
	if err != nil || id == "" {
		t.Fatalf("publishTerminalTo: id=%q err=%v", id, err)
	}
	if wakeErr != nil {
		t.Fatalf("wakeErr = %v, want nil (self-target must never attempt a wake)", wakeErr)
	}
	if got := store.Get("owner/repo-1"); len(got.Tasks) != 0 {
		t.Fatalf("tasks = %+v, want untouched (still down)", got.Tasks)
	}
}

// TestRunScopeUp exercises the no-workflow fallback: a session that names no
// workflow at all falls back to any produced run-scoped task-state entry,
// matching runScopeUp's pre-current-plan behavior.
func TestRunScopeUp(t *testing.T) {
	cases := []struct {
		name  string
		tasks map[string]*contract.TaskState
		want  bool
	}{
		{"nil tasks", nil, false},
		{"only session-scoped produced", map[string]*contract.TaskState{
			"slack_thread": {Scope: contract.TaskScopeSession, Status: contract.TaskStatusProduced},
		}, false},
		{"run-scoped cleaned", map[string]*contract.TaskState{
			"tmux": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusCleaned},
		}, false},
		{"run-scoped produced", map[string]*contract.TaskState{
			"tmux": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		}, true},
	}
	cfg := &config.Config{}
	for _, c := range cases {
		s := &domain.Session{Tasks: c.tasks}
		if got := runScopeUp(cfg, s); got != c.want {
			t.Errorf("%s: runScopeUp = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRunScopeUp_StaleProducedNodeAloneIsDown pins the current-plan fix: a
// task-state entry for a node the frozen workflow no longer declares must
// not read the session as up on its own.
func TestRunScopeUp_StaleProducedNodeAloneIsDown(t *testing.T) {
	cfg := currentPlanConfig(t, "true", "true") // declares only "pane" and "agent"
	s := &domain.Session{
		Workflow: "default",
		Tasks: map[string]*contract.TaskState{
			"removed_node": {Scope: contract.TaskScopeRun, TaskID: "removed_node", Status: contract.TaskStatusProduced},
		},
	}
	if runScopeUp(cfg, s) {
		t.Fatal("runScopeUp = true, want false (the only produced entry is for a node the workflow no longer declares)")
	}
}
