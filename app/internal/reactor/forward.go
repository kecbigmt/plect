package reactor

import (
	"context"
	"log/slog"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/service"
	"github.com/kecbigmt/plecture/app/internal/sessionhub"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// forwardConsumer is the down-forwarder's own durable cursor kind, distinct
// from reactorConsumer and dispatch's "delivery": it drains only Inbound
// events, to relay them rather than tick or deliver.
const forwardConsumer = "resourceforward"

// sessionForwarder relays a down-but-not-destroyed session's new Inbound
// events to its nearest live ancestor, since no sessionReactor drains that
// session's own log while it is down.
type sessionForwarder struct {
	session string
	cfg     *config.Config
	state   *state.Store
	log     *eventlog.Store
	hub     *sessionhub.Registry
	logger  *slog.Logger
	// forwardFn defaults to service.ForwardDownSessionEvent; overridable in tests.
	forwardFn func(*config.Config, *state.Store, string, event.Event) (bool, error)
}

func (f *sessionForwarder) effectiveLogger() *slog.Logger {
	if f.logger != nil {
		return f.logger
	}
	return slog.Default()
}

func (f *sessionForwarder) run(ctx context.Context) {
	seedForwardCursor(f.log, f.session)
	startGen, _ := f.log.StreamID(f.session)
	wake := f.hub.Watch(f.session)
	defer wake.Close()
	fallback := time.NewTicker(fallbackDrain)
	defer fallback.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s, err := f.state.GetE(f.session)
		if err != nil {
			// An unreadable store must not read as "destroyed" and exit for good.
			f.effectiveLogger().Error("forward: read session state failed", "session", f.session, "error", err)
		} else if s == nil {
			return // destroyed
		} else if !f.cfg.RunScopeUp(s) {
			f.drain(ctx, &startGen)
		}
		// Nothing to do while up — the supervisor cancels this goroutine.
		select {
		case <-ctx.Done():
			return
		case <-wake.Wake():
		case <-fallback.C:
		}
	}
}

// seedForwardCursor seeds the cursor at the log's tail if none exists yet
// (mirrors reactor.go's seedCursor), so pre-existing history is never
// replayed as forwards on first use.
func seedForwardCursor(log *eventlog.Store, session string) {
	if log.HasCursor(session, forwardConsumer) {
		return
	}
	_, _, end, err := log.List(session, 0, event.Filter{})
	if err != nil {
		slog.Default().Warn("forward: seed cursor: list failed; cursor left unseeded, next start will retry", "session", session, "error", err)
		return
	}
	if err := log.CommitCursor(session, forwardConsumer, end); err != nil {
		slog.Default().Warn("forward: seed cursor: commit failed; cursor left unseeded, next start will retry", "session", session, "error", err)
	}
}

// drain relays every Inbound event past the committed cursor, advancing the
// cursor one event at a time (mirroring dispatch) so a crash mid-batch
// replays at most one event, not the whole batch.
func (f *sessionForwarder) drain(ctx context.Context, startGen *string) {
	if g, _ := f.log.StreamID(f.session); *startGen != "" && g != *startGen {
		if err := f.log.CommitCursor(f.session, forwardConsumer, 0); err != nil {
			f.effectiveLogger().Warn("forward: reset cursor after log rotation failed", "session", f.session, "error", err)
		}
		*startGen = g
	}
	cur, err := f.log.ReadCursor(f.session, forwardConsumer)
	if err != nil {
		f.effectiveLogger().Warn("forward: read cursor failed; skipping this drain, will retry on next wake", "session", f.session, "error", err)
		return
	}
	evs, offs, next, err := f.log.List(f.session, cur, event.Filter{Direction: event.Inbound})
	if err != nil {
		f.effectiveLogger().Warn("forward: list events failed; skipping this drain, will retry on next wake", "session", f.session, "error", err)
		return
	}
	fn := f.forwardFn
	if fn == nil {
		fn = service.ForwardDownSessionEvent
	}
	for i, ev := range evs {
		if ctx.Err() != nil {
			return
		}
		if _, err := fn(f.cfg, f.state, f.session, ev); err != nil {
			f.effectiveLogger().Warn("forward: relay event failed; will retry on next wake", "session", f.session, "event_id", ev.ID, "error", err)
			return // leave the cursor before ev so it is retried
		}
		commit := next
		if i+1 < len(offs) {
			commit = offs[i+1]
		}
		// A commit failure leaves ev relayed but not advanced past; safe to
		// re-relay next drain since the push dedups on ev's own id.
		if err := f.log.CommitCursor(f.session, forwardConsumer, commit); err != nil {
			f.effectiveLogger().Warn("forward: commit cursor failed; event may re-relay on next drain", "session", f.session, "offset", commit, "error", err)
		}
	}
}
