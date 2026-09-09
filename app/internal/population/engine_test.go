package population

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/lang"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

type hookRecorder struct {
	store         *state.Store
	key           string
	ups           []string
	destroys      []string
	blockers      []string
	admissionSeen bool
	sessionUp     bool
}

func (h *hookRecorder) hooks() Hooks {
	return Hooks{
		Up: func(_ context.Context, resource string, _ map[string]any) (UpOutcome, error) {
			persisted, _ := h.store.Population(h.key)
			h.admissionSeen = persisted != nil && persisted.Members[resource] != nil
			h.ups = append(h.ups, resource)
			return UpOutcome{SessionName: "session-" + resource, AlreadyUp: h.sessionUp}, nil
		},
		Destroy: func(_ context.Context, session, _ string, _ bool) error {
			h.destroys = append(h.destroys, session)
			return nil
		},
		Blockers: func(context.Context, string) ([]string, error) { return h.blockers, nil },
	}
}

func engineFixture(t *testing.T, autoDestroy bool) (*Engine, *hookRecorder, time.Time) {
	t.Helper()
	cfg := populationConfig(t, `[source.query.poll]
type = "exec"
command = "true"
[source.query.subscribe]
type = "exec"
command = "true"
`, "uses = [\"poll\", \"subscribe\"]\npoll_every = \"1m\"", `resource = { from = "resource.id" }`)
	defs, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defs[0].Population.AutoDestroy = autoDestroy
	store := state.NewStore(t.TempDir())
	recorder := &hookRecorder{store: store, key: populationKey(defs[0])}
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	engine := NewEngine(defs[0], store, eventlog.NewStore(store.Dir()), recorder.hooks())
	engine.now = func() time.Time { return now }
	return engine, recorder, now
}

func TestAppearanceIsPersistedBeforeAdmission(t *testing.T) {
	engine, hooks, _ := engineFixture(t, false)
	if err := engine.ApplyAppearance(context.Background(), map[string]any{"resource": "urn:case:a"}); err != nil {
		t.Fatalf("ApplyAppearance: %v", err)
	}
	if !hooks.admissionSeen || len(hooks.ups) != 1 {
		t.Fatalf("persisted before admission = %v, ups = %v", hooks.admissionSeen, hooks.ups)
	}
}

func TestMalformedPollSnapshotChangesNoMembership(t *testing.T) {
	engine, hooks, _ := engineFixture(t, true)
	if err := engine.ApplyPoll(context.Background(), []map[string]any{{"resource": "urn:case:a"}}); err != nil {
		t.Fatal(err)
	}
	err := engine.ApplyPoll(context.Background(), []map[string]any{{"resource": "urn:case:b"}, {"context": "missing resource"}})
	if err == nil {
		t.Fatal("malformed snapshot succeeded")
	}
	if len(hooks.ups) != 1 || len(hooks.destroys) != 0 {
		t.Fatalf("mutations after invalid snapshot: ups=%v destroys=%v", hooks.ups, hooks.destroys)
	}
	state, _ := engine.state.Population(engine.key)
	if state.Members["urn:case:a"].Tombstoned || state.Members["urn:case:b"] != nil {
		t.Fatalf("membership changed: %+v", state.Members)
	}
}

func TestMissingOptionalItemProjectionInvalidatesCompletePollBeforeMutation(t *testing.T) {
	engine, hooks, _ := engineFixture(t, false)
	engine.definition.Population.Session.Inputs["resource"] = &lang.Value{Form: lang.FormFrom, From: "item.context"}
	err := engine.ApplyPoll(context.Background(), []map[string]any{
		{"resource": "urn:case:a", "context": "first"},
		{"resource": "urn:case:b"},
	})
	if err == nil {
		t.Fatal("poll with a missing optional projection succeeded")
	}
	population, stateErr := engine.state.Population(engine.key)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if population != nil || len(hooks.ups) != 0 {
		t.Fatalf("population = %+v, ups = %v, want no mutation", population, hooks.ups)
	}
}

func TestPollAbsenceTombstonesBeforeDryRunAndSuppressesSubscribe(t *testing.T) {
	engine, hooks, _ := engineFixture(t, false)
	var logs bytes.Buffer
	engine.logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	if err := engine.ApplyPoll(ctx, []map[string]any{{"resource": "urn:case:a"}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplyPoll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	state, _ := engine.state.Population(engine.key)
	if !state.Members["urn:case:a"].Tombstoned || len(hooks.destroys) != 0 {
		t.Fatalf("state=%+v destroys=%v", state.Members["urn:case:a"], hooks.destroys)
	}
	if err := engine.ApplyAppearance(ctx, map[string]any{"resource": "urn:case:a"}); err != nil {
		t.Fatal(err)
	}
	if len(hooks.ups) != 1 {
		t.Fatalf("tombstoned appearance admitted: %v", hooks.ups)
	}
	if !strings.Contains(logs.String(), "subscribe appearance suppressed by poll tombstone") {
		t.Fatalf("suppression log = %q", logs.String())
	}
	events, _, _, err := engine.log.List("session-urn:case:a", 0, event.Filter{Types: []string{event.TypeWorkflowPopulationDestroyDryRun}})
	if err != nil || len(events) != 1 {
		t.Fatalf("dry-run events = %v, %v", events, err)
	}
}

// TestUsesSwitchLiftsPollTombstoneSuppression covers the reload case where an
// entry's `uses` selection drops poll: a resource poll tombstoned under the
// old selection would otherwise never see another positive poll snapshot to
// reopen it, so the new selection's subscribe authority must be able to
// admit it directly instead of treating the stale poll tombstone as binding.
func TestUsesSwitchLiftsPollTombstoneSuppression(t *testing.T) {
	engine, hooks, _ := engineFixture(t, false)
	ctx := context.Background()
	if err := engine.ApplyPoll(ctx, []map[string]any{{"resource": "urn:case:a"}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ApplyPoll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	state, _ := engine.state.Population(engine.key)
	if !state.Members["urn:case:a"].Tombstoned {
		t.Fatalf("resource not tombstoned by poll absence: %+v", state.Members["urn:case:a"])
	}

	// Simulates the supervisor recreating the evaluator after a config
	// reload that changes this entry's `uses`: same key, same persisted
	// state, only the selection differs.
	engine.definition.Population.Uses = []string{"subscribe"}
	before := len(hooks.ups)
	if err := engine.ApplyAppearance(ctx, map[string]any{"resource": "urn:case:a"}); err != nil {
		t.Fatal(err)
	}
	if len(hooks.ups) != before+1 {
		t.Fatalf("switching away from poll did not admit the previously tombstoned resource: %v", hooks.ups)
	}
	state, _ = engine.state.Population(engine.key)
	if state.Members["urn:case:a"].Tombstoned {
		t.Fatal("resource still tombstoned after admission under the new selection")
	}
}

func TestPollPositiveReopensTombstoneAndAutoDestroyHonorsBlockers(t *testing.T) {
	engine, hooks, _ := engineFixture(t, true)
	ctx := context.Background()
	if err := engine.ApplyPoll(ctx, []map[string]any{{"resource": "urn:case:a"}}); err != nil {
		t.Fatal(err)
	}
	hooks.blockers = []string{"initial"}
	if err := engine.ApplyPoll(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if len(hooks.destroys) != 0 {
		t.Fatal("blocked member was destroyed")
	}
	hooks.blockers = nil
	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(hooks.destroys) != 1 {
		t.Fatalf("destroys = %v", hooks.destroys)
	}
	if err := engine.ApplyPoll(ctx, []map[string]any{{"resource": "urn:case:a"}}); err != nil {
		t.Fatal(err)
	}
	if len(hooks.ups) != 2 {
		t.Fatalf("positive poll did not reopen generation: %v", hooks.ups)
	}
}

func TestUpFailureLeavesAcceptedAppearancePending(t *testing.T) {
	engine, _, accepted := engineFixture(t, false)
	engine.hooks.Up = func(context.Context, string, map[string]any) (UpOutcome, error) {
		return UpOutcome{}, errors.New("capacity full")
	}
	if err := engine.ApplyAppearance(context.Background(), map[string]any{"resource": "urn:case:a"}); err == nil {
		t.Fatal("expected admission failure")
	}
	state, _ := engine.state.Population(engine.key)
	if !state.Members["urn:case:a"].PendingUp {
		t.Fatalf("member = %+v, want pending", state.Members["urn:case:a"])
	}
	if !state.Members["urn:case:a"].AcceptedAt.IsZero() {
		t.Fatalf("AcceptedAt = %v, want zero while admission is pending", state.Members["urn:case:a"].AcceptedAt)
	}

	created := accepted.Add(30 * time.Minute)
	engine.now = func() time.Time { return created }
	engine.hooks.Up = func(context.Context, string, map[string]any) (UpOutcome, error) {
		return UpOutcome{SessionName: "session-urn:case:a"}, nil
	}
	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = engine.state.Population(engine.key)
	if !state.Members["urn:case:a"].AcceptedAt.Equal(created) {
		t.Fatalf("AcceptedAt = %v, want successful creation time %v", state.Members["urn:case:a"].AcceptedAt, created)
	}
}

// The reason tag this asserts is read back by pendingExistingAhead
// (capacity.go), not consumed anywhere in this file.
func TestAdmitFailureReasonsDrivePriorityClassification(t *testing.T) {
	engine, _, _ := engineFixture(t, false)
	ctx := context.Background()

	if err := engine.ApplyAppearance(ctx, map[string]any{"resource": "urn:case:a"}); err != nil {
		t.Fatal(err)
	}

	engine.hooks.Up = func(context.Context, string, map[string]any) (UpOutcome, error) {
		return UpOutcome{}, &service.Error{Code: service.ErrChildCapExceeded, Message: "cap"}
	}
	if err := engine.ApplyAppearance(ctx, map[string]any{"resource": "urn:case:a"}); err == nil {
		t.Fatal("expected capacity failure")
	}
	events, _, _, err := engine.log.List("session-urn:case:a", 0, event.Filter{Types: []string{event.TypeWorkflowPopulationFailure}})
	if err != nil || len(events) != 1 || events[0].Metadata["reason"] != "capacity" {
		t.Fatalf("failure events = %v, %v, want a single capacity-tagged failure", events, err)
	}

	engine.hooks.Up = func(context.Context, string, map[string]any) (UpOutcome, error) {
		return UpOutcome{}, errors.New("boom")
	}
	if err := engine.ApplyAppearance(ctx, map[string]any{"resource": "urn:case:a"}); err == nil {
		t.Fatal("expected non-capacity failure")
	}
	events, _, _, err = engine.log.List("session-urn:case:a", 0, event.Filter{Types: []string{event.TypeWorkflowPopulationFailure}})
	if err != nil || len(events) != 2 || events[1].Metadata["reason"] != "up" {
		t.Fatalf("failure events = %v, %v, want the second failure tagged \"up\"", events, err)
	}

	// ApplyAppearance validates Session.Inputs before persisting, so the
	// only way admit() itself hits this failure is a definition change
	// after the item was already accepted — a direct Reconcile, not another
	// ApplyAppearance, which would re-validate and never reach admit().
	engine.definition.Population.Session.Inputs["blocked"] = &lang.Value{Form: lang.FormFrom, From: "item.missing_field"}
	engine.hooks.Up = func(context.Context, string, map[string]any) (UpOutcome, error) {
		t.Fatal("hooks.Up called despite a session-input resolution failure")
		return UpOutcome{}, nil
	}
	if err := engine.Reconcile(ctx); err == nil {
		t.Fatal("expected session-input resolution failure")
	}
	events, _, _, err = engine.log.List("session-urn:case:a", 0, event.Filter{Types: []string{event.TypeWorkflowPopulationFailure}})
	if err != nil || len(events) != 3 || events[2].Metadata["reason"] != "input" {
		t.Fatalf("failure events = %v, %v, want the third failure tagged \"input\"", events, err)
	}
}

func TestAdmissionProvenanceConflictEmitsConflictEvent(t *testing.T) {
	engine, _, _ := engineFixture(t, false)
	engine.hooks.Up = func(context.Context, string, map[string]any) (UpOutcome, error) {
		return UpOutcome{}, &populationConflictError{session: "preexisting", reason: "owned elsewhere"}
	}
	if err := engine.ApplyAppearance(context.Background(), map[string]any{"resource": "urn:case:a"}); err == nil {
		t.Fatal("expected provenance conflict")
	}
	events, _, _, err := engine.log.List("preexisting", 0, event.Filter{Types: []string{event.TypeWorkflowPopulationConflict}})
	if err != nil || len(events) != 1 {
		t.Fatalf("conflict events = %v, %v", events, err)
	}
}

func TestInitialTaskFailureRetainsOwnedSessionForRetry(t *testing.T) {
	engine, _, _ := engineFixture(t, false)
	engine.definition.Population.Session.Task = "work"
	engine.hooks.EnsureInitial = func(context.Context, string, string, string) error {
		return errors.New("task setup failed")
	}
	if err := engine.ApplyAppearance(context.Background(), map[string]any{"resource": "urn:case:a"}); err == nil {
		t.Fatal("expected initial task failure")
	}
	population, err := engine.state.Population(engine.key)
	if err != nil {
		t.Fatal(err)
	}
	member := population.Members["urn:case:a"]
	if member.SessionName != "session-urn:case:a" || !member.PendingUp || member.AcceptedAt.IsZero() {
		t.Fatalf("member = %+v, want persisted owned session pending task retry", member)
	}
}

func TestSubscribeExpiryUsesAppearanceAndInboundClock(t *testing.T) {
	cfg := populationConfig(t, `[source.query.subscribe]
type = "exec"
command = "true"
`, "uses = [\"subscribe\"]\nexpire_after = \"1h\"", `resource = { from = "resource.id" }`)
	defs, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defs[0].Population.AutoDestroy = true
	store := state.NewStore(t.TempDir())
	recorder := &hookRecorder{store: store, key: populationKey(defs[0])}
	base := time.Now().Add(-2 * time.Hour)
	engine := NewEngine(defs[0], store, eventlog.NewStore(store.Dir()), recorder.hooks())
	engine.now = func() time.Time { return base }
	ctx := context.Background()
	if err := engine.ApplyAppearance(ctx, map[string]any{"resource": "urn:case:a"}); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = engine.log.Append(event.Event{SessionName: "session-urn:case:a", Type: "external.message", Direction: event.Inbound})
	if err != nil {
		t.Fatal(err)
	}
	engine.now = time.Now
	if err := engine.SweepExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if len(recorder.destroys) != 0 {
		t.Fatal("fresh inbound event did not reset expiry")
	}
	engine.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := engine.SweepExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	if len(recorder.destroys) != 1 {
		t.Fatalf("destroys = %v, want one after inbound quiescence", recorder.destroys)
	}
}

func upEventCount(t *testing.T, engine *Engine, session string) int {
	t.Helper()
	events, _, _, err := engine.log.List(session, 0, event.Filter{Types: []string{event.TypeWorkflowPopulationUp}})
	if err != nil {
		t.Fatalf("list up events: %v", err)
	}
	return len(events)
}

func admitOKEventCount(t *testing.T, engine *Engine, session string) int {
	t.Helper()
	events, _, _, err := engine.log.List(session, 0, event.Filter{Types: []string{event.TypeWorkflowPopulationAdmitOK}})
	if err != nil {
		t.Fatalf("list admit_ok events: %v", err)
	}
	return len(events)
}

// An inbound event on a member re-admits it, which re-runs the idempotent Up
// hook; only a hook call that actually took the session from not-up to up is
// a presence change a relay downstream should see.
func TestReadmissionRecordsUpOnlyOnRunStateTransition(t *testing.T) {
	tests := []struct {
		name          string
		sessionWasUp  bool
		wantUpRecords int
	}{
		{name: "session already up", sessionWasUp: true, wantUpRecords: 1},
		{name: "session down and resuming", sessionWasUp: false, wantUpRecords: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, hooks, _ := engineFixture(t, false)
			ctx := context.Background()
			if err := engine.ApplyPoll(ctx, []map[string]any{{"resource": "urn:case:a"}}); err != nil {
				t.Fatal(err)
			}
			if got := upEventCount(t, engine, "session-urn:case:a"); got != 1 {
				t.Fatalf("first admission recorded %d up events, want 1", got)
			}

			hooks.sessionUp = tt.sessionWasUp
			if _, _, _, err := engine.log.Append(event.Event{
				SessionName: "session-urn:case:a",
				Type:        "external.message",
				Direction:   event.Inbound,
			}); err != nil {
				t.Fatal(err)
			}
			if err := engine.SweepExpiry(ctx); err != nil {
				t.Fatal(err)
			}
			if len(hooks.ups) != 2 {
				t.Fatalf("Up hook calls = %v, want the inbound event to re-run it", hooks.ups)
			}
			if got := upEventCount(t, engine, "session-urn:case:a"); got != tt.wantUpRecords {
				t.Fatalf("up events = %d, want %d", got, tt.wantUpRecords)
			}
			// Unlike the presence-only up event above, admit_ok records
			// every successful admit, so it's 2 either way.
			if got := admitOKEventCount(t, engine, "session-urn:case:a"); got != 2 {
				t.Fatalf("admit_ok events = %d, want 2 regardless of presence change", got)
			}
		})
	}
}

// Initial-task setup runs after the session is already up, and its failure
// leaves the member pending for a retry whose Up hook then reports no
// transition — so a record deferred until after setup is lost for good.
func TestUpSurvivesAnInitialTaskFailureAndItsRetry(t *testing.T) {
	engine, hooks, _ := engineFixture(t, false)
	engine.definition.Population.Session.Task = "work"
	engine.hooks.EnsureInitial = func(context.Context, string, string, string) error {
		return errors.New("task setup failed")
	}
	ctx := context.Background()
	if err := engine.ApplyPoll(ctx, []map[string]any{{"resource": "urn:case:a"}}); err == nil {
		t.Fatal("expected initial task failure")
	}

	// The retry's Up hook finds the session the failed attempt left running.
	hooks.sessionUp = true
	engine.hooks.EnsureInitial = func(context.Context, string, string, string) error { return nil }
	if err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	population, err := engine.state.Population(engine.key)
	if err != nil {
		t.Fatal(err)
	}
	if population.Members["urn:case:a"].PendingUp {
		t.Fatal("retry did not complete the admission")
	}
	if got := upEventCount(t, engine, "session-urn:case:a"); got != 1 {
		t.Fatalf("up events = %d, want exactly one across the failure and its retry", got)
	}
}
