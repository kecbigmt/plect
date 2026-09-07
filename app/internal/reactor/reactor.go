package reactor

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// reactorConsumer is this consumer's durable cursor kind — distinct from
// dispatch's "delivery" cursor so the two followers advance independently
// over the same log.
const reactorConsumer = "tick"

// heartbeatConsumer is the heartbeat sweep's own durable read position: how
// far it has scanned for an Inbound event since the last sweep. It is a
// separate cursor from reactorConsumer's own event-driven drain loop —
// narrower (Inbound events only) and read for a different purpose (backoff
// reset, not triggering a tick) — replacing the old
// Session.TickBackoff.LastLogPosition column now that event_cursors already
// holds exactly this shape of per-session read position for the delivery
// and tick consumers.
const heartbeatConsumer = "heartbeat"

// fallbackDrain re-drains even if a wake was missed/coalesced, mirroring
// dispatch's fallback ticker: correctness rests on the durable cursor, so
// this only bounds worst-case latency, not delivery.
const fallbackDrain = 5 * time.Second

// heartbeatInterval is the cadence of the periodic `heartbeat` sweep. It is
// deliberately coarse relative to typical `heartbeat` values (minutes), and
// checked once immediately at startup so a resident restart sweeps any
// already-overdue session promptly rather than waiting a full interval:
// whatever exceeds the freshness ceiling while the resident process was down
// must be swept on recovery, not left waiting for the next regular tick.
const heartbeatInterval = time.Minute

// channelHealthInterval is the cadence of the periodic channel-health sweep
// (service.CheckChannelHealth). It runs unconditionally, unlike heartbeat/
// healthcheck which are gated behind a workflow declaration, because a
// session's event channel can fail regardless of whether it declares
// [tick]/[healthcheck] — and it is deliberately tighter than
// channelHealthFailureAge so the age-based escalation path fires close to
// its threshold instead of lagging a full sweep behind.
const channelHealthInterval = 30 * time.Second

// sessionReactor follows one session's event log and ticks it when a
// declared event pattern matches, the judge builtin fires, or `heartbeat`
// has elapsed since the session's last tick. Its `[tick]` declaration is
// seeded by the supervisor and re-read on every heartbeat sweep
// (refreshTickConfig), unlike dispatch's sessionDispatcher, whose channels
// stay as resolved at build.
type sessionReactor struct {
	session string
	cfg     *config.Config
	// cfgFn returns the daemon's current config, re-read on every heartbeat
	// sweep so this loop recovers from a config that was unresolvable when it
	// started. Nil in tests that inject cfg and tick directly, which is what
	// keeps those cases from re-resolving over their injected declaration.
	cfgFn       func() *config.Config
	state       *state.Store
	log         *eventlog.Store
	hub         *sessionhub.Registry
	tick        config.TickConfig
	healthcheck config.HealthcheckConfig
	observer    task.Observer
	logger      *slog.Logger
	// tickFn defaults to service.TickSession; overridable in tests to observe
	// invocation count/concurrency (AC6) without weakening production
	// behavior — buildReactor never sets it.
	tickFn        func(*config.Config, *state.Store, service.TickParams) (*service.CheckResult, error)
	healthcheckFn func(*config.Config, *state.Store, service.HealthcheckParams) (*service.HealthReport, error)
	// channelHealthFn defaults to service.CheckChannelHealth; overridable in
	// tests to observe invocation without waiting on real wall-clock tickers.
	channelHealthFn func(*config.Config, *state.Store, string) (bool, error)
	// heartbeatEvery defaults to heartbeatInterval; overridable in tests so
	// the heartbeat-sweep test cases don't need to wait a full minute of wall clock.
	heartbeatEvery   time.Duration
	healthcheckEvery time.Duration
	// channelHealthEvery defaults to channelHealthInterval; overridable in
	// tests, mirroring healthcheckEvery.
	channelHealthEvery time.Duration
	// predecessorDone: see run's own comment for why this waits.
	predecessorDone <-chan struct{}
}

// effectiveLogger falls back to slog.Default() for a test-constructed
// sessionReactor that (like a nil cfgFn) skips buildReactor and leaves
// logger unset.
func (r *sessionReactor) effectiveLogger() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}

func (r *sessionReactor) run(ctx context.Context) {
	// The wait below exists because an outgoing sessionForwarder could
	// otherwise still be mid-relay when this reactor starts, forwarding an
	// event only this reactor should handle once up. Seeded before that
	// wait, not after: reactorConsumer's first-ever seed commits "the log's
	// tail right now", so seeding it only once a possibly slow predecessor
	// is confirmed gone would swallow anything arriving during that wait. A
	// session with a prior up period already has this cursor seeded, so
	// ordering is moot there.
	seedCursor(r.log, r.session)
	awaitPredecessor(r.predecessorDone)
	if ctx.Err() != nil {
		return
	}
	startGen, _ := r.log.StreamID(r.session)
	wake := r.hub.Watch(r.session)
	defer wake.Close()
	fallback := time.NewTicker(fallbackDrain)
	defer fallback.Stop()
	interval := r.heartbeatEvery
	if interval <= 0 {
		interval = heartbeatInterval
	}
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()
	healthInterval := r.healthcheckEvery
	if healthInterval <= 0 {
		healthInterval = r.healthcheck.Period.Duration
	}
	var healthcheck <-chan time.Time
	var healthTicker *time.Ticker
	if healthInterval > 0 {
		healthTicker = time.NewTicker(healthInterval)
		defer healthTicker.Stop()
		healthcheck = healthTicker.C
		r.checkHealth(ctx)
	}
	channelHealthEvery := r.channelHealthEvery
	if channelHealthEvery <= 0 {
		channelHealthEvery = channelHealthInterval
	}
	channelHealthTicker := time.NewTicker(channelHealthEvery)
	defer channelHealthTicker.Stop()
	r.checkChannelHealth(ctx)

	// Immediate check, not just on the first tick of heartbeat: a resident
	// process that was down past `heartbeat` must sweep the backlog on
	// restart, not wait up to another full interval.
	r.checkHeartbeat(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		s, err := r.state.GetE(r.session)
		if err != nil {
			// An unreadable store must never read as "destroyed": that would
			// permanently exit this loop (nothing restarts it) instead of
			// retrying once the store recovers. Log and keep the loop alive
			// to try again on the next wake/tick.
			r.effectiveLogger().Error("reactor: read session state failed", "session", r.session, "error", err)
		} else if s == nil {
			return // destroyed
		} else if r.cfg.RunScopeUp(s) {
			r.drain(ctx, &startGen)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake.Wake():
		case <-fallback.C:
		case <-heartbeat.C:
			r.refreshTickConfig()
			r.checkHeartbeat(ctx)
		case <-healthcheck:
			r.checkHealth(ctx)
		case <-channelHealthTicker.C:
			r.checkChannelHealth(ctx)
		}
	}
}

// seedCursor commits the reactor's durable read cursor at the session log's
// current tail, but only if no cursor exists yet — mirroring
// dispatch.SeedCursor exactly (same idempotency and birth-event rationale),
// duplicated rather than shared because the two consumers must stay free to
// evolve independently.
func seedCursor(log *eventlog.Store, session string) {
	if log.HasCursor(session, reactorConsumer) {
		return
	}
	_, _, end, err := log.List(session, 0, event.Filter{})
	if err != nil {
		slog.Default().Warn("reactor: seed cursor: list failed; cursor left unseeded, next start will retry", "session", session, "error", err)
		return
	}
	if err := log.CommitCursor(session, reactorConsumer, end); err != nil {
		slog.Default().Warn("reactor: seed cursor: commit failed; cursor left unseeded, next start will retry", "session", session, "error", err)
	}
}

// drain reads every event past the committed cursor and ticks at most once
// for the whole batch — same-session ticks stay serialized (one goroutine
// owns this session's follow loop) and rapid bursts coalesce into one tick,
// exactly as verification-gate.md's serialization/debounce rule requires.
func (r *sessionReactor) drain(ctx context.Context, startGen *string) {
	if g, _ := r.log.StreamID(r.session); *startGen != "" && g != *startGen {
		if err := r.log.CommitCursor(r.session, reactorConsumer, 0); err != nil {
			slog.Default().Warn("reactor: reset cursor after log rotation failed", "session", r.session, "error", err)
		}
		*startGen = g
	}
	cur, err := r.log.ReadCursor(r.session, reactorConsumer)
	if err != nil {
		slog.Default().Warn("reactor: read cursor failed; skipping this drain, will retry on next wake", "session", r.session, "error", err)
		return
	}
	evs, _, next, err := r.log.List(r.session, cur, event.Filter{})
	if err != nil {
		slog.Default().Warn("reactor: list events failed; skipping this drain, will retry on next wake", "session", r.session, "error", err)
		return
	}
	if len(evs) == 0 {
		return
	}
	triggered := false
	for _, ev := range evs {
		if r.shouldTrigger(ev) {
			triggered = true
		}
	}
	// Advance the cursor unconditionally so a non-matching event is never
	// re-scanned; this consumer never fans out per-event side effects, so
	// unlike dispatch there is no per-event replay window to preserve.
	if err := r.log.CommitCursor(r.session, reactorConsumer, next); err != nil {
		slog.Default().Warn("reactor: commit cursor failed; batch may re-scan on next drain", "session", r.session, "error", err)
	}
	if triggered {
		r.doTick(ctx, service.TickTriggerEvent)
	}
}

// shouldTrigger decides whether ev should cause a tick. The judge builtin is
// checked first because it is declaration-independent. The self-emitted
// exclusion is checked next and wins over any declared `on` pattern, so a
// workflow that declares an overly broad pattern must not make tick re-trigger
// on its own output. Only events that clear both checks are matched against
// the declared patterns.
func (r *sessionReactor) shouldTrigger(ev event.Event) bool {
	if ev.Type == event.TypeChannelError {
		// A delivery that exhausted its retries must not schedule work: the
		// event it failed to deliver is often tick's own announcement, so
		// reacting here closes a loop between the bus and the session at the
		// retry cadence. Checked ahead of the judge builtin because a failed
		// delivery is never itself evidence of progress, whatever it carried.
		return false
	}
	if ev.Type == event.TypeJudgeRecorded {
		return true
	}
	if ev.Type == event.TypeResourceForwarded {
		// A down child's relayed resource event is a builtin core signal,
		// like the judge verdict above — a workflow has no
		// `[tick].on` pattern of its own to declare for it, so it must
		// trigger regardless of what is declared.
		return true
	}
	if isSelfEmitted(ev) {
		return false
	}
	for _, pattern := range r.tick.On {
		if event.MatchType(pattern, ev.Type) {
			return true
		}
	}
	return false
}

// isSelfEmitted reports whether ev is something tick's own actuation
// produces: a terminal push, a lifecycle event accompanying a chain-spawned
// session's own birth, or one of tick's same-session progress markers
// (review_required, escalated, and — critically — kick's user.emit). The
// last one is why this excludes by Source rather than by enumerating types:
// kick's user.emit is otherwise indistinguishable in Type from a genuine
// external user.emit, so a workflow declaring a broad pattern (e.g. "*" or
// "user.*") would otherwise retrigger tick on its own kick output. These are
// excluded from pattern matching regardless of what a workflow declares in
// `[tick].on`.
func isSelfEmitted(ev event.Event) bool {
	if ev.Source == event.SourceTick {
		return true
	}
	switch ev.Type {
	case event.TypeTerminalDone, event.TypeTerminalEscalate, event.TypeTerminalDead:
		return true
	}
	return strings.HasPrefix(ev.Type, event.TypeLifecyclePrefix)
}

// refreshTickConfig re-reads the session's `[tick]` declaration, and the
// config every tick runs against, from the daemon's current config. Resolving
// them once at startup wedges this loop whenever that moment happened to fall
// inside a window where the config home was unresolvable: nothing rebuilds a
// reactor whose session never goes down, so the empty declaration it settled
// for would outlive the failure and stop heartbeat scheduling until the
// process restarted.
//
// A re-read that leaves nothing to schedule is refused while something is
// scheduled now, and so is one that fails outright. Parsing cleanly does not
// make a read true: a workflow file rewritten in place is readable at byte
// states that parse to no declaration at all — zero length mid-truncate, or a
// `[tick]` table whose keys are not written yet — so one sample cannot tell
// "the declaration was removed" from "the declaration is being edited", and
// believing the wrong one re-enters the very wedge this function exists to
// leave. A genuine removal is honored the next time the session starts.
func (r *sessionReactor) refreshTickConfig() {
	if r.cfgFn == nil {
		return
	}
	cfg := r.cfgFn()
	s, err := r.state.GetE(r.session)
	if err != nil {
		slog.Default().Warn("reactor: read session state failed; keeping the previous [tick] declaration", "session", r.session, "error", err)
		return
	}
	if cfg == nil || s == nil {
		return
	}
	tc, err := resolveTickConfig(cfg, s)
	if err != nil {
		slog.Default().Warn("reactor: re-resolve [tick] failed; keeping the previous declaration", "session", r.session, "error", err)
		return
	}
	if schedulesNothing(tc) && !schedulesNothing(r.tick) {
		slog.Default().Warn("reactor: re-resolved [tick] schedules nothing; keeping the previous declaration", "session", r.session)
		return
	}
	r.cfg = cfg
	r.tick = tc
}

// schedulesNothing reports whether tc leaves the reactor with no clock and no
// pattern of its own — the inert state a session cannot be scheduled out of
// without being restarted.
func schedulesNothing(tc config.TickConfig) bool {
	return len(tc.On) == 0 && tc.Heartbeat.Duration <= 0
}

// checkHeartbeat ticks the session when `heartbeat` (scaled by the quiet-tick
// backoff) has elapsed since LastTickAt. A zero LastTickAt (never ticked)
// counts as infinitely overdue, so a freshly produced instance with
// `heartbeat` gets its first observation on the next sweep rather than
// waiting a full period.
func (r *sessionReactor) checkHeartbeat(ctx context.Context) {
	if r.tick.Heartbeat.Duration <= 0 {
		return
	}
	s, err := r.state.GetE(r.session)
	if err != nil {
		slog.Default().Warn("reactor: read session state failed; skipping this heartbeat sweep", "session", r.session, "error", err)
		return
	}
	if s == nil || !r.cfg.RunScopeUp(s) {
		return
	}
	if !s.LastTickAt.IsZero() {
		n := 0
		if s.TickBackoff != nil {
			n = s.TickBackoff.ConsecutiveUnchanged
		}
		lastLogPosition, err := r.log.ReadCursor(r.session, heartbeatConsumer)
		if err != nil {
			slog.Default().Warn("reactor: read heartbeat cursor failed; treating as unset", "session", r.session, "error", err)
		}
		// Peek for inbound before gating: inbound is otherwise only observed
		// inside updateBackoff, which runs only after a tick fires — so a
		// capped interval would hide inbound for the full cap instead of
		// heartbeat.
		if inbound, _ := r.hasInboundSince(lastLogPosition); inbound {
			n = 0
		}
		if time.Since(s.LastTickAt) < config.BackoffInterval(r.tick.Heartbeat.Duration, r.tick.MaxHeartbeatOrDefault(), n) {
			return
		}
	}
	r.doTick(ctx, service.TickTriggerHeartbeat)
}

func (r *sessionReactor) checkHealth(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s, err := r.state.GetE(r.session)
	if err != nil {
		slog.Default().Warn("reactor: read session state failed; skipping this healthcheck sweep", "session", r.session, "error", err)
		return
	}
	if s == nil || !r.cfg.RunScopeUp(s) {
		return
	}
	fn := r.healthcheckFn
	if fn == nil {
		fn = service.HealthcheckSession
	}
	if _, err := fn(r.cfg, r.state, service.HealthcheckParams{SessionName: r.session, Config: r.healthcheck}); err != nil {
		slog.Default().Warn("reactor: healthcheck failed", "session", r.session, "error", err)
	}
}

// checkChannelHealth escalates the session's open event-channel
// validation/delivery failure streak once it crosses the threshold
// (service.CheckChannelHealth owns the streak/threshold/dedup logic; this
// just supplies the periodic tick, mirroring checkHealth).
func (r *sessionReactor) checkChannelHealth(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s, err := r.state.GetE(r.session)
	if err != nil {
		slog.Default().Warn("reactor: read session state failed; skipping this channel-health sweep", "session", r.session, "error", err)
		return
	}
	if s == nil || !r.cfg.RunScopeUp(s) {
		return
	}
	fn := r.channelHealthFn
	if fn == nil {
		fn = service.CheckChannelHealth
	}
	if _, err := fn(r.cfg, r.state, r.session); err != nil {
		slog.Default().Warn("reactor: channel health check failed", "session", r.session, "error", err)
	}
}

// updateBackoff runs right after any tick (doTick, regardless of trigger) and
// decides whether the next interval resets to heartbeat (a change occurred)
// or keeps growing (quiet). "Change" is fingerprint diff or an inbound event
// since the last sweep — self-emitted events are never
// Inbound (see direction normalization in service.EventPublish), so an
// orchestrator publishing to its own session every tick does not reset its
// own backoff.
func (r *sessionReactor) updateBackoff(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	previousPosition, err := r.log.ReadCursor(r.session, heartbeatConsumer)
	if err != nil {
		slog.Default().Warn("reactor: read heartbeat cursor failed; treating as unset", "session", r.session, "error", err)
	}
	fingerprint := r.compositeFingerprint()
	inbound, next := r.hasInboundSince(previousPosition)
	prev := r.lastFingerprint()
	changed := inbound || fingerprint != prev
	if err := r.log.CommitCursor(r.session, heartbeatConsumer, next); err != nil {
		r.effectiveLogger().Warn("reactor: commit heartbeat cursor failed", "session", r.session, "error", err)
	}
	if err := r.state.Update(r.session, func(s *domain.Session) error {
		if s.TickBackoff == nil {
			s.TickBackoff = &contract.TickBackoff{}
		}
		s.TickBackoff.LastFingerprint = fingerprint
		if changed {
			s.TickBackoff.ConsecutiveUnchanged = 0
		} else {
			s.TickBackoff.ConsecutiveUnchanged++
		}
		return nil
	}); err != nil {
		r.effectiveLogger().Warn("reactor: update tick backoff failed", "session", r.session, "error", err)
	}
}

func (r *sessionReactor) lastFingerprint() string {
	s, err := r.state.GetE(r.session)
	if err != nil {
		slog.Default().Warn("reactor: read session state failed; treating last fingerprint as unset", "session", r.session, "error", err)
		return ""
	}
	if s != nil && s.TickBackoff != nil {
		return s.TickBackoff.LastFingerprint
	}
	return ""
}

// hasInboundSince reports whether any Inbound event was appended past since,
// and returns the log's current end offset to persist as the next watermark.
func (r *sessionReactor) hasInboundSince(since int64) (bool, int64) {
	evs, _, next, err := r.log.List(r.session, since, event.Filter{Direction: event.Inbound})
	if err != nil {
		slog.Default().Warn("reactor: scan inbound events failed", "session", r.session, "error", err)
		return false, since
	}
	return len(evs) > 0, next
}

// compositeFingerprint joins every produced instance's done_when fingerprint
// (computed the same way tick/check dedup keys are) into one string
// representing the whole session's observed state — the heartbeat sweep
// operates at session granularity, not per instance.
func (r *sessionReactor) compositeFingerprint() string {
	result, err := service.CheckSession(r.cfg, r.state, service.CheckParams{SessionName: r.session})
	if err != nil {
		slog.Default().Warn("reactor: compute fingerprint failed", "session", r.session, "error", err)
		return ""
	}
	instances := make([]string, len(result.Actions))
	for i, a := range result.Actions {
		instances[i] = a.Instance + "=" + a.Fingerprint
	}
	slices.Sort(instances)
	return strings.Join(instances, "\x00")
}

// doTick actuates a tick regardless of what triggered it (declared `on`
// pattern, judge builtin, or the heartbeat sweep) and always refreshes the
// backoff bookkeeping afterward. This must not live only in checkHeartbeat's
// caller: a reactively-triggered tick (drain, e.g. an inbound event matching
// a declared pattern) advances LastTickAt exactly like a heartbeat-sweep
// tick, so it must reset ConsecutiveUnchanged the same way — otherwise the
// next checkHeartbeat computes its interval off a stale, inflated n and the
// backoff never actually shortens back to heartbeat despite the inbound
// arrival that should have reset it.
func (r *sessionReactor) doTick(ctx context.Context, trigger service.TickTrigger) {
	if ctx.Err() != nil {
		return
	}
	fn := r.tickFn
	if fn == nil {
		fn = service.TickSession
	}
	if _, err := fn(r.cfg, r.state, service.TickParams{SessionName: r.session, Observer: r.observer, Trigger: trigger}); err != nil {
		slog.Default().Warn("reactor: tick failed", "session", r.session, "error", err)
		return
	}
	if r.tick.Heartbeat.Duration > 0 {
		r.updateBackoff(ctx)
	}
}
