package service

import (
	"fmt"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// terminalScanLimit bounds the reverse scan a dedup check uses against the
// target's recent terminal events (P1: event id dedup so a repeated
// goal-loop tick does not re-push the same fact). A session with
// more than this many terminal pushes since the one being deduped would
// re-push — acceptable, since D1/P1 only require idempotent *handling* of a
// duplicate, not that one can never occur.
const terminalScanLimit = 200

// TerminalParams describes a single-hop terminal push (ADR: cross-session
// terminal event propagation, D1-D3).
type TerminalParams struct {
	Type    string // event.TypeTerminalDone | TypeTerminalEscalate | TypeTerminalDead
	Summary string
	Body    string
	// Metadata carries extra fields (e.g. the task instance). MetaOriginSession,
	// MetaRelation, and MetaDedupKey (when DedupKey is set) are stamped by the
	// publisher and must not be set here.
	Metadata map[string]string
	// DedupKey is the idempotency key; a push carrying the same (Type, DedupKey)
	// already recorded on the target within the scan window is skipped.
	DedupKey string
}

// resolveTerminalTarget strips a "root:" pseudo-parent prefix
// (domain.ImplicitRootParent) down to the addressable session it names, so
// every terminal-push path — PublishTerminalToParent (done/escalate) and
// pushDeadReport's ancestor walk (dead) — resolves a literal ParentSession
// value to the same delivery target instead of each re-implementing the
// strip. A plain (non-root:) parent passes through unchanged.
func resolveTerminalTarget(parent string) string {
	if rootTarget, ok := strings.CutPrefix(parent, "root:"); ok {
		return rootTarget
	}
	return parent
}

// PublishTerminalToParent pushes a done/escalate/dead terminal event from
// origin to origin's immediate parent (D1: single-hop; D2/D3: the parent
// decides whether to handle it or forward further). Returns ("", nil, nil)
// when origin has no parent — a root session has no immediate parent to push
// to, which D1 leaves undefined rather than treating as an error. wakeErr is
// non-nil when the target had to be woken and that wake failed; the event was
// still recorded (err is nil), so callers should surface wakeErr as a warning
// rather than fail the caller's own operation on it.
func PublishTerminalToParent(cfg *config.Config, store *state.Store, origin string, p TerminalParams) (id string, wakeErr error, err error) {
	s, err := store.GetE(origin)
	if err != nil {
		return "", nil, err
	}
	if s == nil {
		return "", nil, &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("session %q not found", origin)}
	}
	if s.ParentSession == "" {
		return "", nil, nil
	}
	target := resolveTerminalTarget(s.ParentSession)
	return publishTerminalTo(cfg, store, origin, target, true, p)
}

// publishTerminalTo appends a delivery_mode=push terminal event into target's
// own log (D1: push routing writes into the *receiving* session's partition,
// not the origin's, so the receiver's own dispatcher — which only ever reads
// its own log — delivers it once the receiver's workflow channels include
// the plect.terminal.* type). Metadata carries the origin session and its
// relation to target so readers don't need a second tree lookup.
//
// When wakeIfDown is true and target's run scope is currently down, this also
// best-effort brings it up (D9: delivery must create a handle-or-forward
// opportunity within bounded time regardless of the receiver's run state).
// Up is idempotent (already-produced tasks are skipped), so this is safe even
// if another actor is concurrently bringing target up. wakeIfDown is ignored
// (forced false) whenever target == origin, regardless of what the caller
// passes: a self-targeted push is the subject reporting on its own bad news,
// and waking it on that news would silently undo an operator's explicit
// `down` and keep the session perpetually re-upped. Enforced here rather
// than trusted to every call site, since a self-target push is never itself
// a wake opportunity.
func publishTerminalTo(cfg *config.Config, store *state.Store, origin, target string, wakeIfDown bool, p TerminalParams) (id string, wakeErr error, err error) {
	if target == origin {
		wakeIfDown = false
	}
	if p.DedupKey != "" {
		dup, derr := hasRecentTerminalEvent(store, target, p.Type, p.DedupKey)
		if derr != nil {
			return "", nil, &Error{Code: ErrExecutionFailed, Message: derr.Error()}
		}
		if dup {
			return "", nil, nil
		}
	}
	meta := make(map[string]string, len(p.Metadata)+3)
	for k, v := range p.Metadata {
		meta[k] = v
	}
	meta[event.MetaOriginSession] = origin
	sessions, err := store.AllE()
	if err != nil {
		return "", nil, err
	}
	meta[event.MetaRelation] = string(domain.RelationFromTarget(sessions, target, origin))
	if p.DedupKey != "" {
		meta[event.MetaDedupKey] = p.DedupKey
	}
	summary := p.Summary
	// dead's summary already names origin inline (pushDeadReport); done/escalate
	// summaries are generic done_when text (checkActionForResult) that reads as
	// the target's own event once session_name is rewritten to target.
	if p.Type != event.TypeTerminalDead {
		summary = fmt.Sprintf("%s (from %s)", summary, origin)
	}
	stored, _, _, aerr := eventlog.NewStore(store.Dir()).Append(event.Event{
		SessionName:  target,
		Type:         p.Type,
		Source:       event.SourcePlect,
		Direction:    event.Inbound,
		Summary:      summary,
		Body:         p.Body,
		Metadata:     meta,
		DeliveryMode: event.DeliveryModePush,
	})
	if aerr != nil {
		return "", nil, &Error{Code: ErrExecutionFailed, Message: aerr.Error()}
	}
	if wakeIfDown {
		ts, err := store.GetE(target)
		if err != nil {
			return "", nil, err
		}
		if ts != nil && !runScopeUp(cfg, ts) {
			if _, uerr := Up(cfg, store, UpParams{Identifier: target}); uerr != nil {
				wakeErr = fmt.Errorf("wake %q: %w", target, uerr)
			}
		}
	}
	return stored.ID, wakeErr, nil
}

func hasRecentTerminalEvent(store *state.Store, session, typ, dedupKey string) (bool, error) {
	evs, err := eventlog.NewStore(store.Dir()).Tail(session, event.Filter{Types: []string{typ}}, terminalScanLimit)
	if err != nil {
		return false, err
	}
	for _, ev := range evs {
		if ev.Metadata[event.MetaDedupKey] == dedupKey {
			return true, nil
		}
	}
	return false, nil
}

// runScopeUp reports whether the session's current plan has a produced
// run-scoped node — the same test the dispatch supervisor uses to decide
// whether a session's dispatcher (and therefore its socket/runtime-delivery
// channel) is live. A task-state entry for a node the frozen workflow no
// longer declares does not count, so a stale record left behind by a
// workflow edit cannot read a session as up on its own. When the workflow
// itself does not resolve at all, this falls back to any produced
// run-scoped task-state entry regardless of node membership, so an
// unrelated config problem does not read an otherwise-live session as down.
func runScopeUp(cfg *config.Config, s *domain.Session) bool {
	if s == nil {
		return false
	}
	current, ok := currentPlanRunScopedNodeSet(cfg, s)
	if !ok {
		return anyProducedRunScopedTaskState(s.Tasks)
	}
	for id := range current {
		st := s.Tasks[id]
		if st != nil && st.Scope == contract.TaskScopeRun && st.Status == contract.TaskStatusProduced {
			return true
		}
	}
	return false
}

// anyProducedRunScopedTaskState is runScopeUp's fallback for a session whose
// workflow does not resolve: any produced run-scoped task-state entry at
// all, regardless of whether its node is still declared.
func anyProducedRunScopedTaskState(tasks map[string]*contract.TaskState) bool {
	for _, e := range tasks {
		if e != nil && e.Scope == contract.TaskScopeRun && e.Status == contract.TaskStatusProduced {
			return true
		}
	}
	return false
}
