package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kecbigmt/plecture/app/internal/channel"
	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
)

// Supervisor keeps at most one dispatcher per active session. It is the single
// owner of dispatcher lifecycle: it starts one when a session's run scope comes
// up and cancels it when the run scope goes down (a suspend — the durable cursor
// is untouched, so the next up resumes) or the session is destroyed.
type Supervisor struct {
	// cfg is a getter rather than a fixed *config.Config: buildDispatcher
	// calls it fresh for every session that comes up, so a plugin mounted
	// (or unmounted) after the daemon started is visible on the next up
	// transition instead of requiring a daemon restart. config.Live.Get is
	// the production getter; tests can close over a fixed *config.Config.
	cfg    func() *config.Config
	state  *state.Store
	log    *eventlog.Store
	hub    *sessionhub.Registry
	logger *slog.Logger
	poll   time.Duration
}

// NewSupervisor builds a supervisor over the same event log the bus serves, the
// shared session state, and the shared per-session reader hub.
func NewSupervisor(cfg func() *config.Config, st *state.Store, log *eventlog.Store, hub *sessionhub.Registry) *Supervisor {
	return &Supervisor{cfg: cfg, state: st, log: log, hub: hub, logger: slog.Default(), poll: time.Second}
}

// Run polls session state and reconciles the running dispatchers until ctx ends,
// then cancels and joins all of them.
func (sup *Supervisor) Run(ctx context.Context) {
	active := map[string]context.CancelFunc{}
	// skip caches sessions that definitively declare no channels, so the
	// supervisor stops re-loading their config from disk every tick.
	skip := map[string]bool{}
	var wg sync.WaitGroup
	defer func() {
		for _, cancel := range active {
			cancel()
		}
		wg.Wait()
	}()

	tick := time.NewTicker(sup.poll)
	defer tick.Stop()
	for {
		sup.reconcile(ctx, active, skip, &wg)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (sup *Supervisor) reconcile(ctx context.Context, active map[string]context.CancelFunc, skip map[string]bool, wg *sync.WaitGroup) {
	cfg := sup.cfg()
	sessions := sup.state.All()
	for name, s := range sessions {
		if _, running := active[name]; running || skip[name] || !cfg.RunScopeUp(s) {
			continue
		}
		d, noChannels := sup.buildDispatcher(name, s)
		if d == nil {
			if noChannels {
				skip[name] = true // definitive; a transient load error retries next tick
			}
			continue
		}
		dctx, cancel := context.WithCancel(ctx)
		active[name] = cancel
		wg.Go(func() { d.run(dctx) })
	}
	for name, cancel := range active {
		if s, ok := sessions[name]; !ok || !cfg.RunScopeUp(s) {
			cancel()
			delete(active, name)
		}
	}
	// Forget skip entries for destroyed sessions so a recreate is re-evaluated.
	for name := range skip {
		if _, ok := sessions[name]; !ok {
			delete(skip, name)
		}
	}
}

// buildDispatcher returns a dispatcher for the session, or nil. noChannels is
// true only when the workflow loaded and definitively declares no channels (the
// negative is safe to cache); a transient load error returns false so it retries.
func (sup *Supervisor) buildDispatcher(name string, s *domain.Session) (*sessionDispatcher, bool) {
	if s.Workflow == "" {
		return nil, true
	}
	cfg := sup.cfg()
	workflows, err := cfg.LoadWorkflows(s.WorkspaceDirPath)
	if err != nil {
		return nil, false
	}
	wf, ok := workflows[s.Workflow]
	if !ok {
		return nil, false
	}
	if len(wf.Event.Channel) == 0 {
		return nil, true
	}
	defs, err := cfg.LoadChannels()
	if err != nil {
		return nil, false
	}
	// Validate at dispatch (config only checks it on `workflow show`): a bad
	// `uses`/input no-ops in processEvent, so surface it in the daemon log. Valid
	// channels still deliver.
	if verr := config.ValidateWorkflowChannels(wf, defs); verr != nil {
		sup.logger.Warn("event channels did not validate; some may not deliver",
			"session", name, "workflow", wf.ID, "error", verr)
		_, _, _, _ = sup.log.Append(channel.ChannelValidationErrorEvent(name, wf.ID, verr))
		recordValidationFailure(sup.state, name, verr, time.Now())
	} else {
		recordValidationSuccess(sup.state, name)
	}
	terminal := resolveTerminalOwner(sup.logger, cfg, s, wf)
	return &sessionDispatcher{
		session:  name,
		cfg:      cfg,
		channels: wf.Event.Channel,
		defs:     defs,
		log:      sup.log,
		state:    sup.state,
		hub:      sup.hub,
		policy:   channel.DefaultRetryPolicy(),
		plugins:  cfg.Plugins,
		terminal: terminal,
	}, false
}
