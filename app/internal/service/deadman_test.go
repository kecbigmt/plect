package service

import (
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// runningTasks is the minimal run-scoped-and-produced task map every deadman
// test's origin session needs, so CheckHeartbeatDeadman's own runScopeUp
// gate does not short-circuit the case under test.
func runningTasks() map[string]*contract.TaskState {
	return map[string]*contract.TaskState{
		"initial": {Scope: contract.TaskScopeRun, TaskID: "runner", Status: contract.TaskStatusProduced},
	}
}

// heartbeatTick is a TickConfig declaring only `heartbeat`, the shape every
// deadman test but the backoff-aware one below needs.
func heartbeatTick(d time.Duration) config.TickConfig {
	return config.TickConfig{Heartbeat: config.Duration{Duration: d}}
}

func setLastTickAt(t *testing.T, store *state.Store, name string, at time.Time) {
	t.Helper()
	if err := store.Update(name, func(s *domain.Session) error {
		s.LastTickAt = at
		return nil
	}); err != nil {
		t.Fatalf("set LastTickAt: %v", err)
	}
}

func setConsecutiveUnchanged(t *testing.T, store *state.Store, name string, n int) {
	t.Helper()
	if err := store.Update(name, func(s *domain.Session) error {
		s.TickBackoff = &contract.TickBackoff{ConsecutiveUnchanged: n}
		return nil
	}); err != nil {
		t.Fatalf("set ConsecutiveUnchanged: %v", err)
	}
}

func listEvents(t *testing.T, store *state.Store, session, typ string) []event.Event {
	t.Helper()
	evs, _, _, err := eventlog.NewStore(store.Dir()).List(session, 0, event.Filter{Types: []string{typ}})
	if err != nil {
		t.Fatalf("list %s events on %s: %v", typ, session, err)
	}
	return evs
}

func TestCheckHeartbeatDeadman_NoDeclaredHeartbeatNeverEscalates(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", runningTasks())
	setLastTickAt(t, store, "owner/repo-1", time.Now().Add(-time.Hour))

	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", config.TickConfig{}, time.Now())
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if escalated {
		t.Fatal("escalated with no declared heartbeat, want no-op")
	}
}

func TestCheckHeartbeatDeadman_NotYetStaleDoesNotEscalate(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", runningTasks())
	now := time.Now()
	setLastTickAt(t, store, "owner/repo-1", now.Add(-2*time.Minute)) // < 3x heartbeat below

	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if escalated {
		t.Fatal("escalated before deadmanMultiplier x heartbeat elapsed")
	}
}

func TestCheckHeartbeatDeadman_RunScopeDownNeverEscalates(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", nil) // no run-scoped produced task
	now := time.Now()
	setLastTickAt(t, store, "owner/repo-1", now.Add(-time.Hour))

	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if escalated {
		t.Fatal("escalated for a session whose run scope is down")
	}
}

func TestCheckHeartbeatDeadman_NeverTickedJudgesStalenessFromCreatedAt(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", runningTasks())
	now := time.Now()

	// CreatedAt was stamped ~now by seedSession — well within one window, so
	// a session just brought up gets its grace period even though it has
	// never ticked (LastTickAt is zero).
	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if escalated {
		t.Fatal("escalated for a freshly created session within its grace window")
	}

	// CreatedAt is a recorded fact, set once at insert and never revised
	// (mirroring the persistence layer's own insert-only created_at
	// column), so an aged-past-its-grace-window session is seeded directly
	// with an old CreatedAt rather than mutated into one after the fact.
	if err := store.Put(&domain.Session{
		Name:      "owner/repo-2",
		Workflow:  "",
		Tasks:     runningTasks(),
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	escalated, err = CheckHeartbeatDeadman(cfg, store, "owner/repo-2", heartbeatTick(time.Minute), now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if !escalated {
		t.Fatal("did not escalate for a session that has never ticked well past CreatedAt + 3x heartbeat")
	}
}

// TestCheckHeartbeatDeadman_BackedOffSessionUsesEffectiveIntervalNotBaseHeartbeat
// proves the fix for a false-positive a code reviewer caught: a session that
// legitimately backed off (repeated quiet heartbeat sweeps, no fingerprint
// change) is due its next real tick at heartbeat * 2^n, not bare heartbeat.
// Judging staleness against 3x the unscaled heartbeat would misread that
// ordinary quiet period as a stalled scheduler.
func TestCheckHeartbeatDeadman_BackedOffSessionUsesEffectiveIntervalNotBaseHeartbeat(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", runningTasks())
	now := time.Now()

	tick := config.TickConfig{
		Heartbeat:    config.Duration{Duration: time.Minute},
		MaxHeartbeat: config.Duration{Duration: time.Hour},
	}
	// effective = BackoffInterval(1m, 1h, 3) = 8m; 3x effective = 24m.
	setConsecutiveUnchanged(t, store, "owner/repo-1", 3)

	setLastTickAt(t, store, "owner/repo-1", now.Add(-20*time.Minute)) // > 3x base heartbeat (3m), < 3x effective (24m)
	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", tick, now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if escalated {
		t.Fatal("escalated a session legitimately backed off — false positive against a healthy quiet session")
	}

	setLastTickAt(t, store, "owner/repo-1", now.Add(-25*time.Minute)) // > 3x effective (24m)
	escalated, err = CheckHeartbeatDeadman(cfg, store, "owner/repo-1", tick, now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if !escalated {
		t.Fatal("did not escalate once staleness passed 3x the backed-off effective interval")
	}

	dead := listEvents(t, store, "owner/repo-1", event.TypeTerminalDead)
	if len(dead) != 1 || dead[0].Metadata["effective_interval"] != "8m0s" {
		t.Fatalf("dead events = %+v, want one with effective_interval=8m0s", dead)
	}
}

func TestCheckHeartbeatDeadman_EscalatesToLiveParentAndDedupsWithinEpisode(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", runningTasks())
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")
	now := time.Now()
	setLastTickAt(t, store, "owner/repo-1", now.Add(-time.Hour))

	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if !escalated {
		t.Fatal("did not escalate a stalled session with a live parent")
	}

	escalations := listEvents(t, store, "owner/repo-orchestrator", event.TypeTerminalEscalate)
	if len(escalations) != 1 {
		t.Fatalf("escalation events = %+v, want exactly one", escalations)
	}
	if escalations[0].Metadata["escalation_kind"] != "heartbeat.deadman" {
		t.Fatalf("metadata = %+v, want heartbeat.deadman escalation kind", escalations[0].Metadata)
	}

	// Same outage episode (LastTickAt unchanged): a later sweep must not
	// re-escalate, and must not fall back to an "undeliverable" record on
	// origin either — the live parent was found, the duplicate is simply
	// nothing further to do.
	escalated, err = CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second CheckHeartbeatDeadman: %v", err)
	}
	if escalated {
		t.Fatal("re-escalated within the same outage episode")
	}
	if got := listEvents(t, store, "owner/repo-orchestrator", event.TypeTerminalEscalate); len(got) != 1 {
		t.Fatalf("escalation events after dedup sweep = %+v, want still exactly one", got)
	}
	if got := listEvents(t, store, "owner/repo-1", event.TypeTerminalDead); len(got) != 0 {
		t.Fatalf("undeliverable dead events = %+v, want none: a live parent was found", got)
	}
}

func TestCheckHeartbeatDeadman_ResumingTicksClosesEpisodeThenStallingAgainEscalatesAnew(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-orchestrator", "owner/repo", 1, "", nil)
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", runningTasks())
	setParent(t, store, "owner/repo-1", "owner/repo-orchestrator")
	now := time.Now()
	setLastTickAt(t, store, "owner/repo-1", now.Add(-time.Hour))

	if escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now); err != nil || !escalated {
		t.Fatalf("first escalation: escalated=%v err=%v", escalated, err)
	}

	// Ticks resume: LastTickAt advances, closing the episode.
	resumedAt := now.Add(2 * time.Minute)
	setLastTickAt(t, store, "owner/repo-1", resumedAt)
	if escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), resumedAt); err != nil || escalated {
		t.Fatalf("escalated right after ticks resumed: escalated=%v err=%v", escalated, err)
	}

	// A new stall well past the resumed LastTickAt is a new episode and must
	// escalate again.
	laterNow := resumedAt.Add(time.Hour)
	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), laterNow)
	if err != nil {
		t.Fatalf("second-episode CheckHeartbeatDeadman: %v", err)
	}
	if !escalated {
		t.Fatal("did not escalate a new outage episode after resuming ticks")
	}
	if got := listEvents(t, store, "owner/repo-orchestrator", event.TypeTerminalEscalate); len(got) != 2 {
		t.Fatalf("escalation events = %+v, want exactly two (one per episode)", got)
	}
}

func TestCheckHeartbeatDeadman_NoLiveAncestorRecordsUndeliverableOnOrigin(t *testing.T) {
	store := testStore(t)
	cfg := &config.Config{}
	seedSession(t, store, "owner/repo-1", "owner/repo", 1, "", runningTasks()) // no parent at all
	now := time.Now()
	setLastTickAt(t, store, "owner/repo-1", now.Add(-time.Hour))

	escalated, err := CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now)
	if err != nil {
		t.Fatalf("CheckHeartbeatDeadman: %v", err)
	}
	if !escalated {
		t.Fatal("did not record an undeliverable deadman event for a root session")
	}

	dead := listEvents(t, store, "owner/repo-1", event.TypeTerminalDead)
	if len(dead) != 1 {
		t.Fatalf("dead events on origin = %+v, want exactly one", dead)
	}
	if dead[0].Metadata["escalation_kind"] != "heartbeat.deadman" {
		t.Fatalf("metadata = %+v, want heartbeat.deadman escalation kind", dead[0].Metadata)
	}

	// Same episode: repeated sweep must not duplicate the self-record.
	escalated, err = CheckHeartbeatDeadman(cfg, store, "owner/repo-1", heartbeatTick(time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second CheckHeartbeatDeadman: %v", err)
	}
	if escalated {
		t.Fatal("re-recorded within the same outage episode")
	}
	if got := listEvents(t, store, "owner/repo-1", event.TypeTerminalDead); len(got) != 1 {
		t.Fatalf("dead events after dedup sweep = %+v, want still exactly one", got)
	}
}
