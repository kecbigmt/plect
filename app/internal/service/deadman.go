package service

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
)

// deadmanMultiplier is the small K in "K x heartbeat with no tick means the
// scheduler itself has gone quiet" (2026-08-16 outage: a reactor loop stall
// silenced heartbeat ticks for 3.5+ hours with nothing but a human noticing).
// Fixed rather than declared per workflow: the invariant it guards — the
// scheduler is still running at all — does not vary by workflow, so a config
// knob here would be a speculative extension point with no concrete
// consumer.
const deadmanMultiplier = 3

// CheckHeartbeatDeadman reports and escalates when name's declared
// `heartbeat` has gone unfired for deadmanMultiplier * the session's current
// effective interval. It must be called from outside the per-session tick
// reactor it is watching: a stalled reactor loop cannot report its own
// stall, since the very goroutine that would fire this check is the one
// that stopped running.
//
// The effective interval is heartbeat scaled by the reactor's own quiet-tick
// backoff (config.BackoffInterval, the identical math checkHeartbeat applies
// before deciding whether to tick), not the bare declared heartbeat: a
// session that has legitimately backed off because nothing changed is due
// its next real tick well past one base heartbeat, and judging staleness
// against the unscaled value would misread that ordinary quiet period as a
// stalled scheduler.
//
// A session that has never ticked judges staleness from CreatedAt instead of
// a zero LastTickAt, so a session just brought up gets one full window's
// grace before its silence counts as a stall; a session that has never
// ticked also has no TickBackoff yet, so its effective interval is
// unscaled heartbeat (n=0).
func CheckHeartbeatDeadman(cfg *config.Config, store *state.Store, name string, tick config.TickConfig, now time.Time) (bool, error) {
	heartbeat := tick.Heartbeat.Duration
	if heartbeat <= 0 {
		return false, nil
	}
	s, err := store.GetE(name)
	if err != nil {
		return false, err
	}
	if s == nil || !runScopeUp(cfg, s) {
		return false, nil
	}
	n := 0
	if s.TickBackoff != nil {
		n = s.TickBackoff.ConsecutiveUnchanged
	}
	effective := config.BackoffInterval(heartbeat, tick.MaxHeartbeatOrDefault(), n)

	staleSince := s.LastTickAt
	if staleSince.IsZero() {
		staleSince = s.CreatedAt
	}
	if staleSince.IsZero() || now.Sub(staleSince) < deadmanMultiplier*effective {
		return false, nil
	}
	return pushHeartbeatDeadmanEscalation(cfg, store, name, staleSince, heartbeat, effective, now)
}

// pushHeartbeatDeadmanEscalation delivers the alert to the first live
// ancestor (resolveLiveAncestor), or — lacking one — records a
// plect.terminal.dead event on origin's own log, the owner channel path
// already used by health escalations for a root orchestrator.
//
// The dedup key is keyed on staleSince rather than a persisted notify
// counter: as long as ticks stay stopped, staleSince (LastTickAt) does not
// advance, so every sweep within the same outage episode recomputes the same
// key. Unlike watchdog.go's pushHealthEscalation (which never repeats this
// call for an unchanged state because HealthcheckSession's shouldNotifyHealth
// gates it upstream), CheckHeartbeatDeadman has no such gate and is expected
// to be called every sweep for as long as the outage lasts — so this
// function checks the dedup key itself, before publishing, rather than
// discovering a duplicate only via publishTerminalTo's own scan. That
// distinction matters here specifically because a discovered duplicate must
// stop the attempt outright, not fall through to the undeliverable-fallback
// path as if no live ancestor had been found; resolving the target first and
// checking its dedup state before publishing keeps those two outcomes
// separate. Once ticks resume, LastTickAt advances past staleSince and a
// later stall produces a new key, escalating again as its own new episode.
func pushHeartbeatDeadmanEscalation(cfg *config.Config, store *state.Store, origin string, staleSince time.Time, heartbeat, effective time.Duration, now time.Time) (bool, error) {
	meta := map[string]string{
		"escalation_kind":    "heartbeat.deadman",
		"heartbeat":          heartbeat.String(),
		"effective_interval": effective.String(),
		"stale_since":        staleSince.UTC().Format(time.RFC3339),
	}
	body := heartbeatDeadmanBody(origin, staleSince, heartbeat, effective, now)
	dedupKey := origin + "|heartbeat_deadman|" + staleSince.UTC().Format(time.RFC3339Nano)

	target, err := resolveLiveAncestor(cfg, store, origin)
	if err != nil {
		slog.Warn("resolve live ancestor for heartbeat deadman escalation failed", "session", origin, "error", err)
		return false, err
	}
	if target == "" {
		return publishDeadmanOnce(cfg, store, origin, origin, false, event.TypeTerminalDead,
			fmt.Sprintf("%s heartbeat deadman is undeliverable", origin), body, meta, dedupKey+"|undeliverable")
	}
	return publishDeadmanOnce(cfg, store, origin, target, true, event.TypeTerminalEscalate,
		fmt.Sprintf("%s heartbeat scheduling has gone silent", origin), body, meta, dedupKey)
}

// publishDeadmanOnce checks dedupKey against target's own recent history
// before publishing, so a duplicate sweep within the same outage episode
// reports "nothing to do" rather than a delivery failure or a second,
// differently-typed event.
func publishDeadmanOnce(cfg *config.Config, store *state.Store, origin, target string, wakeIfDown bool, typ, summary, body string, meta map[string]string, dedupKey string) (bool, error) {
	dup, err := hasRecentTerminalEvent(store, target, typ, dedupKey)
	if err != nil {
		return false, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	if dup {
		return false, nil
	}
	id, wakeErr, err := publishTerminalTo(cfg, store, origin, target, wakeIfDown, TerminalParams{
		Type:     typ,
		Summary:  summary,
		Body:     body,
		Metadata: meta,
		DedupKey: dedupKey,
	})
	if err != nil {
		slog.Warn("publish heartbeat deadman escalation failed", "session", origin, "target", target, "error", err)
		return false, err
	}
	if wakeErr != nil {
		slog.Warn("heartbeat deadman escalation delivered but target wake failed", "session", origin, "target", target, "error", wakeErr)
	}
	return id != "", nil
}

// resolveLiveAncestor walks origin's ancestor chain and returns the first
// one whose own health is not itself unhealthy/stalled ("" when no parent or
// no live ancestor exists) — the same selection
// publishHealthEscalationToLiveAncestor makes, factored out as a pure
// resolution (no publish) so the caller can dedup against the resolved
// target before deciding whether to publish at all.
func resolveLiveAncestor(cfg *config.Config, store *state.Store, origin string) (string, error) {
	visited := map[string]bool{origin: true}
	current := origin
	for {
		s, err := store.GetE(current)
		if err != nil {
			return "", err
		}
		if s == nil {
			return "", &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("session %q not found", current)}
		}
		if s.ParentSession == "" {
			return "", nil
		}
		parent := resolveTerminalTarget(s.ParentSession)
		if parent == "" || visited[parent] {
			return "", nil
		}
		visited[parent] = true
		parentHealth, healthErr := EvaluateHealth(cfg, store, parent)
		if healthErr != nil || parentHealth.State() == domain.HealthUnhealthy || parentHealth.State() == domain.HealthStalled {
			current = parent
			continue
		}
		return parent, nil
	}
}

func heartbeatDeadmanBody(origin string, staleSince time.Time, heartbeat, effective time.Duration, now time.Time) string {
	return fmt.Sprintf(
		"%s heartbeat scheduling has gone silent: no tick has fired in %s (heartbeat=%s, effective_interval=%s, threshold=%dx effective_interval).\n\nstale_since: %s",
		origin, now.Sub(staleSince).Round(time.Second), heartbeat, effective, deadmanMultiplier, staleSince.UTC().Format(time.RFC3339),
	)
}
