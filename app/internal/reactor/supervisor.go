// Package reactor drives `plect tick` mechanically instead of leaving it to an
// orchestrator's judgment or memory (docs/wiki/verification-gate.md). It is
// the tick-side sibling of internal/dispatch (channel delivery): both are
// per-session followers of the same durable event log, started and stopped
// by an identical "one goroutine per up session" supervisor. They stay two
// separate packages rather than one merged follower because they answer two
// different, independently evolving questions of the same log — dispatch what
// may be delivered, tick what may be reacted to. Both refuse
// `TypeChannelError`, but as mirror images of one structural rule (a failed
// delivery must not feed whatever produced it), not as a shared policy;
// tick's self-excitation whitelist has no counterpart on the delivery side at
// all. They also stay separate because internal/service already imports
// internal/dispatch (dispatch.SeedCursor, via createsetup.go), so a reactor
// that calls service.TickSession cannot itself live inside internal/dispatch
// without an import cycle.
package reactor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/app/internal/task"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// deadmanInterval is the heartbeat-deadman sweep's own cadence — coarse,
// like reactor.go's own heartbeatInterval, since a deadman threshold is
// always some multiple of a `heartbeat` value itself measured in minutes or
// more.
const deadmanInterval = time.Minute

// Supervisor keeps at most one sessionReactor per active session, starting
// one when a session's run scope comes up and cancelling it when the scope
// goes down or the session is destroyed — the same lifecycle contract as
// dispatch.Supervisor.
type Supervisor struct {
	// cfg is a getter rather than a fixed *config.Config, matching
	// dispatch.Supervisor: buildReactor calls it fresh for every session
	// that comes up, so a plugin-declared workflow mounted after the daemon
	// started is visible on the next up transition instead of requiring a
	// daemon restart.
	cfg      func() *config.Config
	state    *state.Store
	log      *eventlog.Store
	hub      *sessionhub.Registry
	observer task.Observer
	logger   *slog.Logger
	poll     time.Duration
	// deadmanPoll defaults to deadmanInterval; overridable in tests so the
	// heartbeat-deadman sweep test cases don't need to wait a full minute of
	// wall clock.
	deadmanPoll time.Duration
	// deadmanFn defaults to service.CheckHeartbeatDeadman; overridable in
	// tests to observe invocation without depending on real terminal-event
	// delivery, matching sessionReactor's tickFn/healthcheckFn convention.
	deadmanFn func(*config.Config, *state.Store, string, config.TickConfig, time.Time) (bool, error)
}

// NewSupervisor builds a supervisor over the same event log, session state,
// and per-session reader hub the resident process and dispatch.Supervisor share.
func NewSupervisor(cfg func() *config.Config, st *state.Store, log *eventlog.Store, hub *sessionhub.Registry) *Supervisor {
	return &Supervisor{cfg: cfg, state: st, log: log, hub: hub, logger: slog.Default(), poll: time.Second}
}

// Run polls session state and reconciles the running reactors until ctx ends,
// then cancels and joins all of them.
func (sup *Supervisor) Run(ctx context.Context) {
	active := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		for _, cancel := range active {
			cancel()
		}
		wg.Wait()
	}()

	tick := time.NewTicker(sup.poll)
	defer tick.Stop()
	deadmanEvery := sup.deadmanPoll
	if deadmanEvery <= 0 {
		deadmanEvery = deadmanInterval
	}
	deadman := time.NewTicker(deadmanEvery)
	defer deadman.Stop()
	// Swept once immediately, not just on the first tick of deadman: a
	// resident process that was down past a session's deadman threshold must
	// surface that on restart, not wait up to another full interval — same
	// rationale as reactor.go's own immediate checkHeartbeat call.
	sup.checkDeadman(ctx)
	for {
		sup.reconcile(ctx, active, &wg)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-deadman.C:
			sup.checkDeadman(ctx)
		}
	}
}

// checkDeadman sweeps every up session with a declared `heartbeat` for the
// heartbeat-scheduling deadman condition (service.CheckHeartbeatDeadman): K x
// the session's current effective interval elapsed with no tick. This runs
// from Supervisor.Run's own poll loop, never from inside a per-session
// sessionReactor — a stalled reactor loop cannot report its own stall,
// since the very goroutine that would run the check is the one that
// stopped running, so the check must live somewhere a stuck per-session
// reactor cannot also block.
func (sup *Supervisor) checkDeadman(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	fn := sup.deadmanFn
	if fn == nil {
		fn = service.CheckHeartbeatDeadman
	}
	cfg := sup.cfg()
	now := time.Now()
	sessions, err := sup.state.AllE()
	if err != nil {
		sup.logger.Warn("reactor: read session state failed; skipping this deadman sweep", "error", err)
		return
	}
	for name, s := range sessions {
		if !hasRunScopeUp(s.Tasks) {
			continue
		}
		tc, err := resolveTickConfig(cfg, s)
		if err != nil || tc.Heartbeat.Duration <= 0 {
			continue
		}
		if _, err := fn(cfg, sup.state, name, tc, now); err != nil {
			sup.logger.Warn("reactor: heartbeat deadman check failed", "session", name, "error", err)
		}
	}
}

// resolveTickConfig resolves just the declared `[tick]` table for s. It
// duplicates buildReactor's tc-resolution rather than sharing it: buildReactor
// resolves `[healthcheck]` from the same LoadWorkflows call, and merging the
// two would make every caller pay for a result it does not use. A load failure
// is returned rather than folded into an empty result: its callers
// disagree about what an unresolvable config means — the deadman sweep has
// nothing to check, while a running reactor must keep the declaration it
// already had.
func resolveTickConfig(cfg *config.Config, s *domain.Session) (config.TickConfig, error) {
	if s.Workflow == "" {
		return config.TickConfig{}, nil
	}
	workflows, err := cfg.LoadWorkflows(s.WorkspaceDirPath)
	if err != nil {
		return config.TickConfig{}, err
	}
	wf, ok := workflows[s.Workflow]
	if !ok || wf.Tick == nil {
		return config.TickConfig{}, nil
	}
	return *wf.Tick, nil
}

func (sup *Supervisor) reconcile(ctx context.Context, active map[string]context.CancelFunc, wg *sync.WaitGroup) {
	sessions, err := sup.state.AllE()
	if err != nil {
		// An unreadable store must never read as "every session is gone":
		// the loop below would then cancel every currently-running reactor.
		// Skip this cycle entirely and retry on the next poll instead.
		sup.logger.Error("reactor: read session state failed; skipping this reconcile pass", "error", err)
		return
	}
	for name, s := range sessions {
		if _, running := active[name]; running || !hasRunScopeUp(s.Tasks) {
			continue
		}
		r := sup.buildReactor(name, s)
		rctx, cancel := context.WithCancel(ctx)
		active[name] = cancel
		wg.Go(func() { r.run(rctx) })
	}
	for name, cancel := range active {
		if s, ok := sessions[name]; !ok || !hasRunScopeUp(s.Tasks) {
			cancel()
			delete(active, name)
		}
	}
}

// buildReactor seeds the session's `[tick]`/`[healthcheck]` declarations for
// a reactor that is about to start; the reactor re-reads `[tick]` for itself
// from there on (sessionReactor.refreshTickConfig), so a load failure here is
// recoverable rather than terminal. It degrades to an empty TickConfig rather
// than abstaining entirely: the judge builtin trigger is
// declaration-independent, so a session must still be watched for it even
// when its workflow config cannot be resolved.
func (sup *Supervisor) buildReactor(name string, s *domain.Session) *sessionReactor {
	cfg := sup.cfg()
	var tc config.TickConfig
	hc := config.DefaultHealthcheckConfig()
	if s.Workflow != "" {
		workflows, err := cfg.LoadWorkflows(s.WorkspaceDirPath)
		if err != nil {
			sup.logger.Warn("reactor: load workflows failed; declared [tick]/heartbeat inactive, judge builtin still active", "session", name, "error", err)
		} else if wf, ok := workflows[s.Workflow]; ok {
			if wf.Tick != nil {
				tc = *wf.Tick
			}
			hc = config.NormalizeHealthcheckConfig(wf.Healthcheck)
		}
	}
	return &sessionReactor{
		session:     name,
		cfg:         cfg,
		cfgFn:       sup.cfg,
		state:       sup.state,
		log:         sup.log,
		hub:         sup.hub,
		tick:        tc,
		healthcheck: hc,
		observer:    sup.observer,
		logger:      sup.logger,
	}
}

func hasRunScopeUp(tasks map[string]*contract.TaskState) bool {
	for _, e := range tasks {
		if e != nil && e.Scope == contract.TaskScopeRun && e.Status == contract.TaskStatusProduced {
			return true
		}
	}
	return false
}
