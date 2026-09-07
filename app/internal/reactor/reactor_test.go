package reactor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/persistence"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

type onceBuiltBinaries struct {
	once sync.Once
	dir  string
	err  error
}

func (o *onceBuiltBinaries) build(root string, binaries []struct{ moduleDir, pkg, name string }, env []string) (string, error) {
	o.once.Do(func() {
		dir, err := os.MkdirTemp("", "plect-shared-bin-")
		if err != nil {
			o.err = err
			return
		}
		o.dir = dir
		for _, b := range binaries {
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, b.name), b.pkg)
			cmd.Dir = filepath.Join(root, b.moduleDir)
			cmd.Env = append(os.Environ(), env...)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				o.err = fmt.Errorf("build %s: %w", b.name, err)
				return
			}
		}
	})
	return o.dir, o.err
}

func (o *onceBuiltBinaries) cleanup() {
	if o.dir != "" {
		os.RemoveAll(o.dir)
	}
}

var sharedShippedPluginBinaries onceBuiltBinaries

func TestMain(m *testing.M) {
	code := m.Run()
	sharedShippedPluginBinaries.cleanup()
	os.Exit(code)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeClaudeRunTask declares the run-scoped effect a "claude" node uses,
// under root/tasks — cfg.RunScopeUp resolves a produced "claude" task-state
// entry against this declaration to decide the node is current-plan
// run-scoped.
func writeClaudeRunTask(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "tasks", "claude.toml"), `
[claude]
kind  = "effect"
scope = "run"
`)
}

// newTestReactor builds a sessionReactor over a fast-poll hub so tests don't
// wait on the production 500ms poll interval.
func newTestReactor(t *testing.T, tc config.TickConfig) (*sessionReactor, *state.Store, *eventlog.Store) {
	t.Helper()
	dir := t.TempDir()
	log := eventlog.NewStore(dir)
	hub := sessionhub.NewRegistry(log, sessionhub.WithPollInterval(2*time.Millisecond))
	t.Cleanup(hub.Close)
	st := state.NewStore(dir)
	if err := st.Put(&domain.Session{
		Name: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}
	r := &sessionReactor{
		session: "o/r-1",
		cfg:     &config.Config{WorkspaceDirsRoot: t.TempDir()},
		state:   st,
		log:     log,
		hub:     hub,
		tick:    tc,
	}
	return r, st, log
}

// startReactor starts r.run and blocks until its cursor/Watch are seeded, so callers need no fixed sleep despite SQLite's variable first-touch cost.
func startReactor(t *testing.T, r *sessionReactor) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.run(ctx); close(done) }()
	waitForCursorSeed(t, r.log, r.session)
	return func() {
		cancel()
		<-done
	}
}

// waitForCursorSeed polls for run()'s seedCursor commit; see startReactor.
func waitForCursorSeed(t *testing.T, log *eventlog.Store, session string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !log.HasCursor(session, reactorConsumer) {
		if time.Now().After(deadline) {
			t.Fatal("reactor never seeded its cursor")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitLastTickAt polls until the session's LastTickAt is at or after floor
// (the moment just before the triggering append), or fails the test.
func waitLastTickAt(t *testing.T, st *state.Store, session string, floor time.Time) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := st.Get(session); s != nil && !s.LastTickAt.Before(floor) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %q was never ticked (LastTickAt did not advance past %s)", session, floor)
}

// assertNeverTicked sleeps briefly and asserts LastTickAt never advanced past
// floor — used to prove a self-emitted event did not trigger a tick.
func assertNeverTicked(t *testing.T, st *state.Store, session string, floor time.Time) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if s := st.Get(session); s != nil && s.LastTickAt.After(floor) {
		t.Fatalf("session %q was ticked (LastTickAt = %s) when it should not have been", session, s.LastTickAt)
	}
}

func TestSessionReactor_TicksOnDeclaredPattern(t *testing.T) {
	r, st, log := newTestReactor(t, config.TickConfig{On: []string{"resource.*"}})
	stop := startReactor(t, r)
	defer stop()

	floor := time.Now()
	log.Append(event.Event{SessionName: "o/r-1", Type: "resource.updated", Direction: event.Internal})
	waitLastTickAt(t, st, "o/r-1", floor)
}

func TestSessionReactor_UndeclaredWorkflowDoesNotReactiveTick(t *testing.T) {
	// AC3: no `[tick].on` declared → an event that would otherwise match
	// nothing (empty On) must not cause a reactive tick.
	r, st, log := newTestReactor(t, config.TickConfig{})
	stop := startReactor(t, r)
	defer stop()

	floor := time.Now()
	log.Append(event.Event{SessionName: "o/r-1", Type: "resource.updated", Direction: event.Internal})
	assertNeverTicked(t, st, "o/r-1", floor)
}

// TestSessionReactor_SelfEmittedEventsNeverTrigger proves the self-excitation
// whitelist wins over an intentionally maximal declared pattern (AC5, ADR
// amendment 2026-07-04 §3): tick's own terminal/progress markers, a kick's
// user.emit, and a chain-spawn's lifecycle event must not re-trigger tick
// even under `on = ["*"]`. The kick case is the one that must exclude by
// Source rather than Type: its Type (`user.emit`) is otherwise
// indistinguishable from a genuine externally published user.emit.
func TestSessionReactor_SelfEmittedEventsNeverTrigger(t *testing.T) {
	selfEmitted := []event.Event{
		{Type: event.TypeTerminalDone, Source: event.SourcePlect},
		{Type: event.TypeTerminalEscalate, Source: event.SourcePlect},
		{Type: event.TypeTerminalDead, Source: event.SourcePlect},
		{Type: event.TypeTickReviewRequired, Source: event.SourceTick},
		{Type: event.TypeTickEscalated, Source: event.SourceTick},
		{Type: event.TypeUserEmit, Source: event.SourceTick}, // tick's own kick output
		{Type: event.TypeLifecyclePrefix + "created", Source: event.SourcePlect},
	}
	for _, fixture := range selfEmitted {
		t.Run(fixture.Type+"/"+fixture.Source, func(t *testing.T) {
			r, st, log := newTestReactor(t, config.TickConfig{On: []string{"*"}})
			stop := startReactor(t, r)
			defer stop()

			floor := time.Now()
			ev := fixture
			ev.SessionName = "o/r-1"
			log.Append(ev)
			assertNeverTicked(t, st, "o/r-1", floor)
		})
	}
}

// TestSessionReactor_GenuineUserEmitStillTriggersWhenDeclared proves the
// Source-based exclusion doesn't overreach: a user.emit NOT authored by tick
// itself (e.g. published by an orchestrator or a real user) must still
// trigger a declared `on` pattern — only tick's own SourceTick-stamped
// user.emit (kick) is excluded.
func TestSessionReactor_GenuineUserEmitStillTriggersWhenDeclared(t *testing.T) {
	r, st, log := newTestReactor(t, config.TickConfig{On: []string{"user.emit"}})
	stop := startReactor(t, r)
	defer stop()

	floor := time.Now()
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeUserEmit, Source: event.SourceCLI, Direction: event.Internal})
	waitLastTickAt(t, st, "o/r-1", floor)
}

// TestSessionReactor_JudgeRecordedAlwaysTriggers proves the judge builtin
// trigger (AC2) fires with no `[tick].on` declared at all — the trigger is
// plect's own concept, not a declaration.
func TestSessionReactor_JudgeRecordedAlwaysTriggers(t *testing.T) {
	r, st, log := newTestReactor(t, config.TickConfig{})
	stop := startReactor(t, r)
	defer stop()

	floor := time.Now()
	log.Append(event.Event{SessionName: "o/r-1", Type: event.TypeJudgeRecorded, Direction: event.Internal})
	waitLastTickAt(t, st, "o/r-1", floor)
}

// TestSessionReactor_HeartbeatSweepTicksAfterElapsed proves a session with no
// `on` declared still ticks once `heartbeat` has elapsed since its last
// tick, using a shortened heartbeatInterval so the test doesn't wait a full
// minute.
func TestSessionReactor_HeartbeatSweepTicksAfterElapsed(t *testing.T) {
	r, st, _ := newTestReactor(t, config.TickConfig{Heartbeat: config.Duration{Duration: 20 * time.Millisecond}})

	floor := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Drive checkHeartbeat directly on a short interval instead of running the
		// full 1-minute-cadence loop, so the test stays fast without weakening
		// the assertion: checkHeartbeat is exactly what heartbeat.C invokes.
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.checkHeartbeat(ctx)
			}
		}
	}()
	defer func() { cancel(); <-done }()

	waitLastTickAt(t, st, "o/r-1", floor)
}

// TestSessionReactor_UndeclaredHeartbeatNeverSweeps proves that with no
// `heartbeat` declared, the passage of time alone must never tick a session
// (only a declared `on` match or the judge builtin can).
// heartbeatEvery is shortened so several sweep cycles fit in a fast test.
func TestSessionReactor_UndeclaredHeartbeatNeverSweeps(t *testing.T) {
	r, st, _ := newTestReactor(t, config.TickConfig{})
	r.heartbeatEvery = 5 * time.Millisecond
	stop := startReactor(t, r)
	defer stop()

	time.Sleep(80 * time.Millisecond) // several sweep cycles' worth
	if s := st.Get("o/r-1"); s != nil && !s.LastTickAt.IsZero() {
		t.Fatalf("session was ticked (LastTickAt = %s) with no heartbeat declared", s.LastTickAt)
	}
}

func TestSessionReactor_HealthcheckRunsWithoutTickDeclaration(t *testing.T) {
	r, _, _ := newTestReactor(t, config.TickConfig{})
	r.healthcheck = config.HealthcheckConfig{
		Period:         config.Duration{Duration: 20 * time.Millisecond},
		StallThreshold: config.Duration{Duration: time.Minute},
		RenotifyEvery:  3,
	}
	r.healthcheckEvery = 5 * time.Millisecond

	var mu sync.Mutex
	healthchecks := 0
	ticks := 0
	r.healthcheckFn = func(cfg *config.Config, store *state.Store, params service.HealthcheckParams) (*service.HealthReport, error) {
		mu.Lock()
		defer mu.Unlock()
		healthchecks++
		return &service.HealthReport{SessionName: params.SessionName, Healthy: true, Declared: true}, nil
	}
	r.tickFn = func(cfg *config.Config, store *state.Store, params service.TickParams) (*service.CheckResult, error) {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		return &service.CheckResult{}, nil
	}

	stop := startReactor(t, r)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		gotHealthchecks := healthchecks
		gotTicks := ticks
		mu.Unlock()
		if gotHealthchecks > 0 {
			if gotTicks != 0 {
				t.Fatalf("tick calls = %d, want 0 while healthcheck runs without a tick declaration", gotTicks)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("healthcheck was not called")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSessionReactor_ChannelHealthRunsWithoutTickOrHealthcheckDeclaration
// proves the channel-health sweep is unconditional, unlike heartbeat/
// alive: a session's event channel can fail regardless of whether it
// declares [tick] or [healthcheck].
func TestSessionReactor_ChannelHealthRunsWithoutTickOrHealthcheckDeclaration(t *testing.T) {
	r, _, _ := newTestReactor(t, config.TickConfig{})
	r.channelHealthEvery = 5 * time.Millisecond

	var mu sync.Mutex
	calls := 0
	var gotSession string
	r.channelHealthFn = func(cfg *config.Config, store *state.Store, sessionName string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		gotSession = sessionName
		return false, nil
	}

	stop := startReactor(t, r)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := calls
		mu.Unlock()
		if got > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("channel health check was not called")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gotSession != "o/r-1" {
		t.Errorf("channel health check session = %q, want %q", gotSession, "o/r-1")
	}
}

// TestSessionReactor_ReactiveTickResetsHeartbeatWindow proves that a reactive
// tick resets the heartbeat clock, so a `heartbeat` sweep due to fire soon
// after it must not fire an extra, redundant tick within
// that same window — but the sweep must still fire once a full window has
// elapsed since the reset.
func TestSessionReactor_ReactiveTickResetsHeartbeatWindow(t *testing.T) {
	const heartbeat = 120 * time.Millisecond
	r, _, log := newTestReactor(t, config.TickConfig{
		On:        []string{"resource.*"},
		Heartbeat: config.Duration{Duration: heartbeat},
	})
	r.heartbeatEvery = 10 * time.Millisecond

	var mu sync.Mutex
	var calls []time.Time
	r.tickFn = func(cfg *config.Config, store *state.Store, params service.TickParams) (*service.CheckResult, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		return service.TickSession(cfg, store, params) // real stamp, so LastTickAt genuinely resets
	}
	callCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}

	stop := startReactor(t, r)
	defer stop()

	// Wait past the startup heartbeat check (immediate, since LastTickAt starts
	// zero) — this is the first call, establishing a real reset point.
	deadline := time.Now().Add(2 * time.Second)
	for callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if callCount() != 1 {
		t.Fatalf("calls after startup = %d, want exactly 1", callCount())
	}

	// A reactive tick well inside the heartbeat window resets it (the second call).
	log.Append(event.Event{SessionName: "o/r-1", Type: "resource.updated", Direction: event.Internal})
	deadline = time.Now().Add(2 * time.Second)
	for callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if callCount() != 2 {
		t.Fatalf("calls after reactive trigger = %d, want exactly 2", callCount())
	}

	// Within heartbeat of the reset, no extra sweep tick must fire.
	time.Sleep(heartbeat / 2)
	if got := callCount(); got != 2 {
		t.Fatalf("calls within the reset window = %d, want still 2 (heartbeat sweep must not double up)", got)
	}

	// Once a full window has elapsed since the reset, the sweep fires again.
	deadline = time.Now().Add(2 * time.Second)
	for callCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if callCount() != 3 {
		t.Fatalf("calls after the heartbeat window elapsed = %d, want exactly 3", callCount())
	}
}

// TestSessionReactor_TicksSerializeAndDebounceBursts covers AC6 with a
// deterministic (not timing-dependent) proof: a burst of rapid triggers for
// one session coalesces into *exactly one* tick invocation, and no two
// invocations for the same session ever run concurrently.
//
// Determinism, not a race against the drain loop's timing, is what makes
// this airtight: the reactor's durable cursor is pre-committed to the log's
// head (0) *before* the reactor ever starts, so seedCursor's "only seed a
// birth cursor if none exists yet" no-ops — the reactor is guaranteed to
// read every one of the n burst events, appended before it starts, in its
// very first drain() call. drain() triggers at most once per call
// regardless of how many matching events it read (reactor.go's `triggered`
// accumulator), so this setup makes "one coalesced tick for the burst" a
// certainty rather than a probable outcome of scheduling luck.
func TestSessionReactor_TicksSerializeAndDebounceBursts(t *testing.T) {
	r, _, log := newTestReactor(t, config.TickConfig{On: []string{"resource.*"}})

	var mu sync.Mutex
	calls := 0
	inFlight := 0
	maxInFlight := 0
	r.tickFn = func(cfg *config.Config, store *state.Store, params service.TickParams) (*service.CheckResult, error) {
		mu.Lock()
		calls++
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // widen the window so an overlap would be observed
		mu.Lock()
		inFlight--
		mu.Unlock()
		return &service.CheckResult{}, nil
	}

	const n = 20
	for range n {
		log.Append(event.Event{SessionName: "o/r-1", Type: "resource.updated", Direction: event.Internal})
	}

	// Pre-committing to 0 stops seedCursor from seeding past this burst, so the reactor's first drain() reads all n events in one batch.
	if err := log.CommitCursor("o/r-1", reactorConsumer, 0); err != nil {
		t.Fatal(err)
	}

	stop := startReactor(t, r)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for func() int { mu.Lock(); defer mu.Unlock(); return calls }() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give a second wake/fallback cycle a chance to run so a bug that split
	// the burst across two drains would show up as calls == 2, not be missed
	// by stopping too early.
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight > 1 {
		t.Fatalf("tick ran concurrently for the same session: maxInFlight = %d", maxInFlight)
	}
	if calls != 1 {
		t.Fatalf("burst of %d rapid triggers coalesced into %d tick invocation(s), want exactly 1", n, calls)
	}
}

func TestSupervisor_StartsAndStopsWithRunScope(t *testing.T) {
	cfg := &config.Config{}
	st := state.NewStore(t.TempDir())
	if err := st.Put(&domain.Session{
		Name: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, st, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	sup.reconcile(ctx, active, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("reactor not started for an up session")
	}
	sup.reconcile(ctx, active, &wg) // idempotent: no duplicate
	if len(active) != 1 {
		t.Fatalf("expected exactly one reactor, got %d", len(active))
	}

	// Run scope goes down → supervisor cancels (suspend, not teardown).
	if err := st.Update("o/r-1", func(s *domain.Session) error {
		s.Nodes["claude"].Status = contract.TaskStatusCleaned
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sup.reconcile(ctx, active, &wg)
	if len(active) != 0 {
		t.Fatalf("reactor not stopped after run scope down: %d active", len(active))
	}
}

// TestSupervisor_ReconcileDoesNotCancelActiveReactorsWhenStoreUnreadable
// proves the fix for a real hazard: reconcile's second loop cancels any
// active reactor whose session is absent from AllE's result, so a swallowed
// read error returning an empty map would previously cancel every reactor
// currently running, not just the ones truly destroyed.
func TestSupervisor_ReconcileDoesNotCancelActiveReactorsWhenStoreUnreadable(t *testing.T) {
	cfg := &config.Config{}
	st := state.NewStore(t.TempDir())
	if err := st.Put(&domain.Session{
		Name: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return cfg }, st, log, hub)
	ctx := t.Context()
	active := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer func() {
		for _, c := range active {
			c()
		}
	}()

	sup.reconcile(ctx, active, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("reactor not started for an up session")
	}

	// Swap in a store whose database file is unreadable, simulating the
	// store becoming unreadable mid-flight.
	brokenDir := t.TempDir()
	writeFile(t, persistence.PathIn(brokenDir), "not a database")
	sup.state = state.NewStore(brokenDir)

	sup.reconcile(ctx, active, &wg)
	if _, ok := active["o/r-1"]; !ok {
		t.Fatal("reconcile cancelled an active reactor when the store became unreadable, treating an error as \"destroyed\"")
	}
}

// TestSupervisor_ResolvesTickConfigFromPluginMountedAfterConstruction mirrors
// dispatch.Supervisor's identical fix: a `[tick]` declaration that exists
// only in a plugin-only workflow (no global copy) must resolve once the
// supervisor's config getter reflects the plugin, even though the getter
// returned a Config without that plugin when the Supervisor was built —
// matching a long-running daemon whose config.Live refresh mounts a plugin
// after `plect serve` started, not just after a restart.
func TestSupervisor_ResolvesTickConfigFromPluginMountedAfterConstruction(t *testing.T) {
	pluginDir := t.TempDir()
	writeFile(t, filepath.Join(pluginDir, "config", "workflows", "coding.toml"), `
[coding]
kind = "workflow"
[coding.tick]
on = ["github.*"]
`)

	// currentCfg starts without the plugin mounted, as if the daemon started
	// before this plugin was enabled; the getter closure below always reads
	// its current value, so reassigning it simulates config.Live swapping in
	// a freshly-resolved Config on its next periodic refresh.
	currentCfg := &config.Config{}
	st := state.NewStore(t.TempDir())
	if err := st.Put(&domain.Session{
		Name: "o/r-1", Workflow: "coding",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}
	log := eventlog.NewStore(t.TempDir())
	hub := sessionhub.NewRegistry(log)
	defer hub.Close()
	sup := NewSupervisor(func() *config.Config { return currentCfg }, st, log, hub)

	// The plugin mounts (config refresh happens) before this session's
	// reactor is built.
	currentCfg = &config.Config{PluginDirs: []string{pluginDir}}

	r := sup.buildReactor("o/r-1", st.Get("o/r-1"))
	if len(r.tick.On) != 1 || r.tick.On[0] != "github.*" {
		t.Fatalf("tick config = %+v, want on=[\"github.*\"] resolved from the plugin-only workflow", r.tick)
	}
}

// TestBackoffInterval covers the pure doubling/cap arithmetic the quiet-tick
// backoff relies on: heartbeat * 2^n, capped at max, well-defined for n far
// beyond what would overflow a naive bit-shift.
func TestBackoffInterval(t *testing.T) {
	base := 15 * time.Minute
	max := 4 * time.Hour
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, 15 * time.Minute},
		{1, 30 * time.Minute},
		{2, 1 * time.Hour},
		{4, 4 * time.Hour},   // 15m*2^4 = 4h exactly: at the cap, not over it
		{5, 4 * time.Hour},   // would be 8h uncapped
		{100, 4 * time.Hour}, // far beyond overflow of a naive 1<<n
	}
	for _, c := range cases {
		if got := config.BackoffInterval(base, max, c.n); got != c.want {
			t.Errorf("BackoffInterval(n=%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

// TestSessionReactor_QuietTickBackoffGrows proves a session with no
// fingerprint change and no inbound event backs off exponentially: each
// heartbeat sweep's tick pushes the next required interval further out, up to
// max_heartbeat.
func TestSessionReactor_QuietTickBackoffGrows(t *testing.T) {
	const heartbeat = 10 * time.Millisecond
	const maxHeartbeat = 40 * time.Millisecond
	r, st, _ := newTestReactor(t, config.TickConfig{
		Heartbeat:    config.Duration{Duration: heartbeat},
		MaxHeartbeat: config.Duration{Duration: maxHeartbeat},
	})
	tickCount := 0
	r.tickFn = func(cfg *config.Config, store *state.Store, params service.TickParams) (*service.CheckResult, error) {
		tickCount++
		return service.TickSession(cfg, store, params)
	}
	ctx := context.Background()

	// First sweep: never ticked (LastTickAt zero) → ticks immediately. There is no
	// prior fingerprint/inbound baseline, so the empty-vs-empty comparison
	// reads as "unchanged" and ConsecutiveUnchanged becomes 1.
	r.checkHeartbeat(ctx)
	if tickCount != 1 {
		t.Fatalf("first sweep: tickCount = %d, want 1", tickCount)
	}
	if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n != 1 {
		t.Fatalf("first sweep: ConsecutiveUnchanged = %d, want 1", n)
	}

	// Force each subsequent sweep's elapsed time past the current backoff
	// interval by rewinding LastTickAt, so the test doesn't wait real
	// wall-clock multiples of heartbeat.
	rewindAndSweep := func(n int) {
		interval := config.BackoffInterval(heartbeat, maxHeartbeat, n)
		if err := st.Update("o/r-1", func(s *domain.Session) error {
			s.LastTickAt = time.Now().Add(-interval - time.Millisecond)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.checkHeartbeat(ctx)
	}

	rewindAndSweep(1)
	if tickCount != 2 {
		t.Fatalf("second sweep: tickCount = %d, want 2", tickCount)
	}
	n2 := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged
	if n2 != 2 {
		t.Fatalf("second sweep: ConsecutiveUnchanged = %d, want 2 (still quiet)", n2)
	}

	rewindAndSweep(n2)
	if tickCount != 3 {
		t.Fatalf("third sweep: tickCount = %d, want 3", tickCount)
	}
	n3 := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged
	if n3 != 3 {
		t.Fatalf("third sweep: ConsecutiveUnchanged = %d, want 3", n3)
	}

	// Before the backed-off interval elapses, a sweep must not tick.
	if err := st.Update("o/r-1", func(s *domain.Session) error {
		s.LastTickAt = time.Now().Add(-heartbeat - time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tickCount = 0
	r.checkHeartbeat(ctx)
	if tickCount != 0 {
		t.Fatalf("sweep before interval elapsed ticked (tickCount=%d)", tickCount)
	}
}

func TestSessionReactor_BackoffResetLiveChildrenHoldsIntervalWhileChildUp(t *testing.T) {
	const heartbeat = 10 * time.Millisecond
	r, st, _ := newTestReactor(t, config.TickConfig{
		Heartbeat:    config.Duration{Duration: heartbeat},
		BackoffReset: []string{"inbound", "fingerprint", "live_children"},
	})
	tickCount := 0
	r.tickFn = func(cfg *config.Config, store *state.Store, params service.TickParams) (*service.CheckResult, error) {
		tickCount++
		return service.TickSession(cfg, store, params)
	}
	if err := st.Put(&domain.Session{
		Name:          "o/r-1-child",
		ParentSession: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	sweep := func() {
		if err := st.Update("o/r-1", func(s *domain.Session) error {
			s.LastTickAt = time.Now().Add(-heartbeat - time.Millisecond)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.checkHeartbeat(ctx)
	}

	for i := 1; i <= 3; i++ {
		sweep()
		if tickCount != i {
			t.Fatalf("sweep %d: tickCount = %d, want %d", i, tickCount, i)
		}
		if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n != 0 {
			t.Fatalf("sweep %d: ConsecutiveUnchanged = %d, want 0 (held by the live child)", i, n)
		}
	}
}

// A condition is peeked before gating, not only persisted after a tick fires.
func TestSessionReactor_BackoffResetLiveChildrenOverridesCappedIntervalOnceChildComesUp(t *testing.T) {
	const heartbeat = 10 * time.Millisecond
	const maxHeartbeat = 200 * time.Millisecond
	r, st, _ := newTestReactor(t, config.TickConfig{
		Heartbeat:    config.Duration{Duration: heartbeat},
		MaxHeartbeat: config.Duration{Duration: maxHeartbeat},
		BackoffReset: []string{"inbound", "fingerprint", "live_children"},
	})
	tickCount := 0
	r.tickFn = func(cfg *config.Config, store *state.Store, params service.TickParams) (*service.CheckResult, error) {
		tickCount++
		return service.TickSession(cfg, store, params)
	}
	ctx := context.Background()

	// n=4's capped interval (160ms) is far beyond the heartbeat (10ms) elapsed below.
	if err := st.Update("o/r-1", func(s *domain.Session) error {
		s.TickBackoff = &contract.TickBackoff{ConsecutiveUnchanged: 4}
		s.LastTickAt = time.Now().Add(-heartbeat - time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.checkHeartbeat(ctx)
	if tickCount != 0 {
		t.Fatalf("sweep before any child is up: tickCount = %d, want 0 (capped interval not yet elapsed)", tickCount)
	}

	if err := st.Put(&domain.Session{
		Name:          "o/r-1-child",
		ParentSession: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}

	r.checkHeartbeat(ctx)
	if tickCount != 1 {
		t.Fatalf("sweep once the child is up: tickCount = %d, want 1 (due at heartbeat, not the capped interval)", tickCount)
	}
	if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n != 0 {
		t.Fatalf("ConsecutiveUnchanged after the tick = %d, want 0 (persisted reset)", n)
	}
}

func TestSessionReactor_BackoffResetLiveChildrenGrowsOnceChildIsDown(t *testing.T) {
	const heartbeat = 10 * time.Millisecond
	const maxHeartbeat = 40 * time.Millisecond
	r, st, _ := newTestReactor(t, config.TickConfig{
		Heartbeat:    config.Duration{Duration: heartbeat},
		MaxHeartbeat: config.Duration{Duration: maxHeartbeat},
		BackoffReset: []string{"inbound", "fingerprint", "live_children"},
	})
	r.tickFn = service.TickSession
	if err := st.Put(&domain.Session{
		Name:          "o/r-1-child",
		ParentSession: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusCleaned},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	rewindAndSweep := func(n int) {
		interval := config.BackoffInterval(heartbeat, maxHeartbeat, n)
		if err := st.Update("o/r-1", func(s *domain.Session) error {
			s.LastTickAt = time.Now().Add(-interval - time.Millisecond)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.checkHeartbeat(ctx)
	}

	rewindAndSweep(0)
	if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n != 1 {
		t.Fatalf("first sweep with the child already down: ConsecutiveUnchanged = %d, want 1", n)
	}
	rewindAndSweep(1)
	if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n != 2 {
		t.Fatalf("second sweep with the child already down: ConsecutiveUnchanged = %d, want 2 — the interval doubles as today", n)
	}
}

func TestSessionReactor_BackoffResetDefaultIgnoresLiveChildren(t *testing.T) {
	const heartbeat = 10 * time.Millisecond
	const maxHeartbeat = 40 * time.Millisecond
	r, st, _ := newTestReactor(t, config.TickConfig{
		Heartbeat:    config.Duration{Duration: heartbeat},
		MaxHeartbeat: config.Duration{Duration: maxHeartbeat},
	})
	r.tickFn = service.TickSession
	if err := st.Put(&domain.Session{
		Name:          "o/r-1-child",
		ParentSession: "o/r-1",
		Nodes: map[string]*contract.TaskState{
			"claude": {Scope: contract.TaskScopeRun, Status: contract.TaskStatusProduced},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	rewindAndSweep := func(n int) {
		interval := config.BackoffInterval(heartbeat, maxHeartbeat, n)
		if err := st.Update("o/r-1", func(s *domain.Session) error {
			s.LastTickAt = time.Now().Add(-interval - time.Millisecond)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.checkHeartbeat(ctx)
	}

	rewindAndSweep(0)
	if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n != 1 {
		t.Fatalf("first sweep: ConsecutiveUnchanged = %d, want 1 despite the live child — live_children is not in the default reset set", n)
	}
	rewindAndSweep(1)
	if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n != 2 {
		t.Fatalf("second sweep: ConsecutiveUnchanged = %d, want 2 — the interval doubles as today", n)
	}
}

// TestSessionReactor_QuietTickBackoffResetsOnInbound proves AC3/AC5: an
// inbound event since the last sweep resets ConsecutiveUnchanged to 0 even
// though nothing else changed, so the next interval is heartbeat again.
func TestSessionReactor_QuietTickBackoffResetsOnInbound(t *testing.T) {
	const heartbeat = 10 * time.Millisecond
	r, st, log := newTestReactor(t, config.TickConfig{Heartbeat: config.Duration{Duration: heartbeat}})
	r.tickFn = service.TickSession
	ctx := context.Background()

	// Seed a few quiet sweeps so ConsecutiveUnchanged > 0.
	r.checkHeartbeat(ctx)
	for range 2 {
		n := 0
		if s := st.Get("o/r-1"); s.TickBackoff != nil {
			n = s.TickBackoff.ConsecutiveUnchanged
		}
		interval := config.BackoffInterval(heartbeat, config.DefaultMaxHeartbeat, n)
		if err := st.Update("o/r-1", func(s *domain.Session) error {
			s.LastTickAt = time.Now().Add(-interval - time.Millisecond)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.checkHeartbeat(ctx)
	}
	if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n == 0 {
		t.Fatalf("expected backoff to have grown before the inbound event, got n=%d", n)
	}

	// An inbound event lands (e.g. a subscribed github.* change or another
	// session's publish) — direction normalization guarantees these are
	// Inbound, never a same-session self-publish.
	if _, _, _, err := log.Append(event.Event{
		SessionName: "o/r-1",
		Type:        "github.state",
		Source:      "github",
		Direction:   event.Inbound,
	}); err != nil {
		t.Fatal(err)
	}

	n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged
	interval := config.BackoffInterval(heartbeat, config.DefaultMaxHeartbeat, n)
	if err := st.Update("o/r-1", func(s *domain.Session) error {
		s.LastTickAt = time.Now().Add(-interval - time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.checkHeartbeat(ctx)
	if got := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; got != 0 {
		t.Fatalf("ConsecutiveUnchanged = %d after inbound event, want 0 (reset)", got)
	}
}

// TestSessionReactor_QuietTickBackoffResetsOnInbound_WithoutForcedElapse
// proves inbound is detected on the next sweep even when only heartbeat
// (not the capped interval) has elapsed — unlike the sibling test above,
// which forces the elapsed time past whatever interval is already current.
func TestSessionReactor_QuietTickBackoffResetsOnInbound_WithoutForcedElapse(t *testing.T) {
	const heartbeat = 10 * time.Millisecond
	const maxHeartbeat = 40 * time.Millisecond
	r, st, log := newTestReactor(t, config.TickConfig{
		Heartbeat:    config.Duration{Duration: heartbeat},
		MaxHeartbeat: config.Duration{Duration: maxHeartbeat},
	})
	r.tickFn = service.TickSession
	ctx := context.Background()

	// Force the backoff straight to the capped interval, as a long-quiet
	// production session would reach after many sweeps.
	if err := st.Update("o/r-1", func(s *domain.Session) error {
		s.LastTickAt = time.Now()
		s.TickBackoff = &contract.TickBackoff{ConsecutiveUnchanged: 10}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := log.Append(event.Event{
		SessionName: "o/r-1",
		Type:        "github.state",
		Source:      "github",
		Direction:   event.Inbound,
	}); err != nil {
		t.Fatal(err)
	}

	// Only heartbeat has elapsed, not the capped interval.
	if err := st.Update("o/r-1", func(s *domain.Session) error {
		s.LastTickAt = time.Now().Add(-heartbeat - time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.checkHeartbeat(ctx)
	if got := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; got != 0 {
		t.Fatalf("ConsecutiveUnchanged = %d after inbound event with only heartbeat elapsed, want 0 (reset, tick fired)", got)
	}
}

// TestSessionReactor_DrainTriggeredInboundTickResetsBackoff proves a tick
// fired by drain's reactive path (a declared `on` pattern matching an inbound
// event, not the heartbeat sweep) also resets ConsecutiveUnchanged. doTick is the
// single actuation point for both paths, so a reactive tick must not leave
// heartbeat backoff bookkeeping in place — otherwise the next checkHeartbeat computes
// its interval off an inflated n despite the inbound arrival that should have
// reset it — backoff bookkeeping previously lived only in checkHeartbeat, so
// drain-triggered ticks skipped it entirely.
func TestSessionReactor_DrainTriggeredInboundTickResetsBackoff(t *testing.T) {
	const heartbeat = 200 * time.Millisecond
	r, st, log := newTestReactor(t, config.TickConfig{
		On:        []string{"resource.*"},
		Heartbeat: config.Duration{Duration: heartbeat},
	})
	r.tickFn = service.TickSession

	// Seeded before start so run()'s own immediate heartbeat check sees a non-zero LastTickAt and takes the interval-gated branch, not racing updateBackoff.
	if err := st.Update("o/r-1", func(s *domain.Session) error {
		s.LastTickAt = time.Now()
		s.TickBackoff = &contract.TickBackoff{ConsecutiveUnchanged: 5}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stop := startReactor(t, r)
	defer stop()

	floor := time.Now()

	// A declared-pattern event that direction normalization guarantees is
	// Inbound for an externally-originated publish triggers a reactive tick
	// via drain, independent of the heartbeat sweep's cadence.
	log.Append(event.Event{SessionName: "o/r-1", Type: "resource.updated", Direction: event.Inbound})
	waitLastTickAt(t, st, "o/r-1", floor)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged; n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ConsecutiveUnchanged never reset after a drain-triggered inbound tick, still %d", st.Get("o/r-1").TickBackoff.ConsecutiveUnchanged)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSessionReactor_ChannelErrorNeverTriggers proves a failed delivery
// cannot feed a tick. plect.channel.error is what a channel worker appends
// after exhausting its retries — including retries of an event tick itself
// published — so triggering on it would close a loop between the bus and the
// session: announce, fail to deliver, tick, announce again, at the retry
// cadence. Dispatch already refuses to deliver this type for the mirror-image
// reason; the reactor must refuse to react to it even under `on = ["*"]`.
func TestSessionReactor_ChannelErrorNeverTriggers(t *testing.T) {
	r, st, log := newTestReactor(t, config.TickConfig{On: []string{"*"}})
	stop := startReactor(t, r)
	defer stop()

	floor := time.Now()
	log.Append(event.Event{
		SessionName: "o/r-1",
		Type:        event.TypeChannelError,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
	})
	assertNeverTicked(t, st, "o/r-1", floor)
}
