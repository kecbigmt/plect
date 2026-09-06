package service

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/kecbigmt/plecture/app/internal/config"
	"github.com/kecbigmt/plecture/app/internal/domain"
	"github.com/kecbigmt/plecture/app/internal/eventlog"
	"github.com/kecbigmt/plecture/app/internal/state"
	"github.com/kecbigmt/plecture/contracts/event"
	contract "github.com/kecbigmt/plecture/contracts/state"
)

// EventPublishParams describes an event to publish to a session's log.
type EventPublishParams struct {
	Type      string
	Source    string
	Direction event.Direction
	Summary   string
	Body      string
	Metadata  map[string]string
}

// EventPublish appends an event to the resolved session's log and returns the
// stored event (with id/time assigned). The session need not exist in state.
func EventPublish(cfg *config.Config, store *state.Store, identifier string, p EventPublishParams) (event.Event, error) {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return event.Event{}, err
	}
	// Writing into a session's log (and, with --relay, injecting a message into
	// its agent) is a per-session write: clamp it to the active session guard so
	// a guarded orchestrator can't publish into another owner's session it can
	// merely see via `plect ls`.
	if guardErr := checkSessionGuard(cfg, name); guardErr != nil {
		return event.Event{}, guardErr
	}
	if p.Type == "" {
		return event.Event{}, &Error{Code: ErrInvalidInput, Message: "event type is required"}
	}
	source := p.Source
	if source == "" {
		source = event.SourceCLI
	}
	direction, meta := normalizePublishDirection(name, p.Direction, p.Metadata)
	stored, _, _, err := eventlog.NewStore(store.Dir()).Append(event.Event{
		SessionName: name,
		Type:        p.Type,
		Source:      source,
		Direction:   direction,
		Summary:     p.Summary,
		Body:        p.Body,
		Metadata:    meta,
	})
	if err != nil {
		return event.Event{}, &Error{Code: ErrExecutionFailed, Message: err.Error()}
	}
	return stored, nil
}

// normalizePublishDirection stamps the publishing session (`PLECT_SESSION_NAME`)
// as event.MetaOriginSession and forces Inbound whenever the origin differs
// from the target session — "direction = whether the origin is outside this
// session" is the single condition the tick reactor's quiet-tick backoff
// needs so it never resets on an orchestrator's own self-publish (which has
// origin == target). An explicit
// caller-supplied direction is honored only for a same-session publish (tick's
// own Internal/Outbound markers) or when there is no ambient session (a
// plain CLI invocation outside a pane, or a caller that already resolved
// direction itself, e.g. the MCP/CLI --direction flag with no env session).
func normalizePublishDirection(target string, requested event.Direction, metadata map[string]string) (event.Direction, map[string]string) {
	origin := os.Getenv("PLECT_SESSION_NAME")
	if origin == "" {
		return requested, metadata
	}
	meta := metadata
	if _, ok := meta[event.MetaOriginSession]; !ok {
		meta = make(map[string]string, len(metadata)+1)
		maps.Copy(meta, metadata)
		meta[event.MetaOriginSession] = origin
	}
	if origin != target {
		return event.Inbound, meta
	}
	return requested, meta
}

// EventRecent returns up to the last `limit` events for a session (oldest
// first), resolving the identifier the same way as the other event functions.
// It bounds the read for long-lived sessions; callers that need the full
// history use EventList.
func EventRecent(cfg *config.Config, store *state.Store, identifier string, limit int) ([]event.Event, error) {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return nil, err
	}
	evs, lerr := eventlog.NewStore(store.Dir()).Tail(name, event.Filter{}, limit)
	if lerr != nil {
		return nil, &Error{Code: ErrExecutionFailed, Message: lerr.Error()}
	}
	return evs, nil
}

// EventPageParams requests one page of a session's event log.
type EventPageParams struct {
	Order  event.Order  // asc (default) or desc
	Cursor string       // opaque token from a prior page's NextCursor; "" = first page
	Filter event.Filter // type/source/direction selection + Limit (page size)
}

// EventPageResult is a page of events plus a forward cursor to resume from.
type EventPageResult struct {
	Events     []event.Event
	NextCursor string // resume watermark after the last event; "" when the page is empty (and always "" for desc in v1)
}

// EventPage returns one page of a session's events using opaque cursors. asc
// (default) paginates forward from the head (or from Cursor) and returns a
// NextCursor; desc returns the most recent page (newest first) and does not
// paginate in v1. A Cursor is validated against the requested order and the
// log's current generation, so an order mismatch or a cursor stale across a log
// rotation is rejected rather than silently resolving to the wrong record.
func EventPage(cfg *config.Config, store *state.Store, identifier string, p EventPageParams) (EventPageResult, error) {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return EventPageResult{}, err
	}
	order := p.Order
	if order == "" {
		order = event.OrderAsc
	}
	log := eventlog.NewStore(store.Dir())
	gen, gerr := log.Gen(name)
	if gerr != nil {
		return EventPageResult{}, &Error{Code: ErrExecutionFailed, Message: gerr.Error()}
	}

	var since int64
	if p.Cursor != "" {
		cur, derr := event.DecodeCursor(p.Cursor)
		if derr != nil {
			return EventPageResult{}, &Error{Code: ErrInvalidInput, Message: derr.Error()}
		}
		if verr := cur.Validate(order, gen); verr != nil {
			return EventPageResult{}, &Error{Code: ErrInvalidInput, Message: verr.Error()}
		}
		since = cur.Off
	}

	if order == event.OrderDesc {
		evs, lerr := log.Tail(name, p.Filter, p.Filter.Limit)
		if lerr != nil {
			return EventPageResult{}, &Error{Code: ErrExecutionFailed, Message: lerr.Error()}
		}
		slices.Reverse(evs)
		return EventPageResult{Events: evs}, nil
	}

	evs, _, next, lerr := log.List(name, since, p.Filter)
	if lerr != nil {
		return EventPageResult{}, &Error{Code: ErrExecutionFailed, Message: lerr.Error()}
	}
	res := EventPageResult{Events: evs}
	// A forward cursor only makes sense once the log exists (gen != ""); without
	// it there is nothing to resume against and a "" cursor signals "no page".
	if gen != "" {
		res.NextCursor = event.Cursor{V: event.CursorVersion, Off: next, Ord: event.OrderAsc, Gen: gen}.Encode()
	}
	return res, nil
}

// EventStreamResume validates a resume cursor via EventPage's own check: an
// offset from one log generation must never be read against another.
func EventStreamResume(cfg *config.Config, store *state.Store, identifier, cursor string) (gen string, offset int64, err error) {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return "", 0, err
	}
	gen, gerr := eventlog.NewStore(store.Dir()).Gen(name)
	if gerr != nil {
		return "", 0, &Error{Code: ErrExecutionFailed, Message: gerr.Error()}
	}
	if cursor == "" {
		return gen, 0, nil
	}
	cur, derr := event.DecodeCursor(cursor)
	if derr != nil {
		return "", 0, &Error{Code: ErrInvalidInput, Message: derr.Error()}
	}
	if verr := cur.Validate(event.OrderAsc, gen); verr != nil {
		return "", 0, &Error{Code: ErrInvalidInput, Message: verr.Error()}
	}
	return gen, cur.Off, nil
}

// EventPageSubtree returns one page of the merged event timeline for the subtree
// rooted at identifier (the root session plus all its descendants), in time
// order by event id. asc (default) pages forward via a ULID keyset cursor and
// returns a resume watermark cursor whenever the page has events (see
// EventPageResult.NextCursor); desc returns the most recent page and does not
// paginate in v1 — mirroring EventPage's single-session contract. Membership is
// the canonical session tree (ADR §7). The root must exist in state: a subtree
// is a tree fact, so a destroyed root (its log survives, its tree does not) has
// no subtree to page.
func EventPageSubtree(cfg *config.Config, store *state.Store, identifier string, p EventPageParams) (EventPageResult, error) {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return EventPageResult{}, err
	}
	sessions, err := store.AllE()
	if err != nil {
		return EventPageResult{}, err
	}
	if sessions[name] == nil {
		return EventPageResult{}, &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("no session %q in state; subtree views need a tree root", name)}
	}
	order := p.Order
	if order == "" {
		order = event.OrderAsc
	}
	var after string
	if p.Cursor != "" {
		cur, derr := event.DecodeSubtreeCursor(p.Cursor)
		if derr != nil {
			return EventPageResult{}, &Error{Code: ErrInvalidInput, Message: derr.Error()}
		}
		if verr := cur.Validate(name, order); verr != nil {
			return EventPageResult{}, &Error{Code: ErrInvalidInput, Message: verr.Error()}
		}
		after = cur.After
	}

	all, lerr := eventlog.NewStore(store.Dir()).ListAcross(domain.Subtree(sessions, name), p.Filter)
	if lerr != nil {
		return EventPageResult{}, &Error{Code: ErrExecutionFailed, Message: lerr.Error()}
	}

	events, lastID := pageMerged(all, order, after, p.Filter.Limit)
	res := EventPageResult{Events: events}
	if lastID != "" {
		res.NextCursor = event.SubtreeCursor{V: event.CursorVersion, Root: name, After: lastID, Ord: event.OrderAsc}.Encode()
	}
	return res, nil
}

// pageMerged applies order + ULID keyset paging over an already id-sorted
// (ascending) merge of events from several logs. after is a prior page's last id
// ("" = from the head). For asc it returns the page and the id to resume after:
// a resume watermark present whenever the page has events, so an incremental
// tailer can persist it and resume with no loss and no duplicates — the empty
// page (not the empty cursor) is the "caught up" signal. desc returns the most
// recent page and no watermark (no pagination in v1).
func pageMerged(all []event.Event, order event.Order, after string, limit int) (events []event.Event, lastID string) {
	if order == event.OrderDesc {
		slices.Reverse(all) // input is ascending by id
		if limit > 0 && len(all) > limit {
			all = all[:limit]
		}
		return all, ""
	}
	// asc: keyset by id — skip everything at or before the cursor, then take the
	// page. ids are ULIDs (lexicographic = chronological), so a string compare is
	// the keyset test.
	start := 0
	if after != "" {
		for start < len(all) && all[start].ID <= after {
			start++
		}
	}
	all = all[start:]
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	if len(all) > 0 {
		return all, all[len(all)-1].ID
	}
	return all, ""
}

// EventTailSubtree follows the events of the subtree rooted at identifier live,
// invoking fn for each event matching f until ctx is done. It re-resolves the
// subtree from state each tick, so a child session spawned after the follow
// started joins automatically (the chaining engine's reviewer sessions, ADR §2).
func EventTailSubtree(ctx context.Context, cfg *config.Config, store *state.Store, identifier string, f event.Filter, fn func(event.Event)) error {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return err
	}
	session, err := store.GetE(name)
	if err != nil {
		return err
	}
	if session == nil {
		return &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("no session %q in state; subtree views need a tree root", name)}
	}
	return eventlog.NewStore(store.Dir()).FollowAcross(ctx, func() ([]string, error) {
		sessions, err := store.AllE()
		if err != nil {
			return nil, err
		}
		return domain.Subtree(sessions, name), nil
	}, f, fn)
}

// EventList returns events for a session at or after byte offset `since` that
// match f, plus their offsets and the next read cursor. Works for destroyed
// sessions too — the log is read directly, independent of state.
func EventList(cfg *config.Config, store *state.Store, identifier string, since int64, f event.Filter) ([]event.Event, []int64, int64, error) {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return nil, nil, 0, err
	}
	evs, offs, next, lerr := eventlog.NewStore(store.Dir()).List(name, since, f)
	if lerr != nil {
		return nil, nil, 0, &Error{Code: ErrExecutionFailed, Message: lerr.Error()}
	}
	return evs, offs, next, nil
}

// EventTail follows a session's events from byte offset `since`, invoking fn
// for each event matching f, until ctx is done.
func EventTail(ctx context.Context, cfg *config.Config, store *state.Store, identifier string, since int64, f event.Filter, fn func(event.Event)) error {
	name, err := resolveSessionName(cfg, store, identifier)
	if err != nil {
		return err
	}
	// Prefer the live bus when one is configured (PLECT_BUS_SOCKET): it follows the
	// same log via SSE with automatic reconnect, and exercises the bus stream
	// path end-to-end. Without it, read the log directly. The bus applies the
	// filter server-side, so only the local path needs to Match.
	if socket := os.Getenv("PLECT_BUS_SOCKET"); socket != "" {
		client := event.NewUDSClient(socket, os.Getenv("PLECT_BUS_TOKEN"))
		return client.Subscribe(ctx, name, since, f, func(ev event.Event, _ int64) {
			fn(ev)
		})
	}
	return eventlog.NewStore(store.Dir()).Follow(ctx, name, since, func(ev event.Event, _ int64) {
		if f.Match(ev) {
			fn(ev)
		}
	})
}

// EventShow returns a single event by id from a session's log.
func EventShow(cfg *config.Config, store *state.Store, identifier, id string) (*event.Event, error) {
	evs, _, _, err := EventList(cfg, store, identifier, 0, event.Filter{})
	if err != nil {
		return nil, err
	}
	for i := range evs {
		if evs[i].ID == id {
			return &evs[i], nil
		}
	}
	return nil, &Error{Code: ErrSessionNotFound, Message: fmt.Sprintf("no event with id %q", id)}
}

// recordSessionCreated records lifecycle.created exactly once per session.
// "new state entry" is not a reliable "first creation" signal: the
// workflow-setup create path intentionally leaves an inspectable state entry on
// a failed setup, so a later retry sees an existing entry yet is the first
// success. Keying off the event itself (record iff no lifecycle.created exists)
// is idempotent across both retries and re-runs of an already-created session.
func recordSessionCreated(store *state.Store, sessionName string) {
	if hasLifecycleEvent(store, sessionName, "created") {
		return
	}
	recordLifecycle(store, sessionName, "created", "session created")
}

// hasLifecycleEvent reports whether the session's log already holds a
// lifecycle.<phase> event.
func hasLifecycleEvent(store *state.Store, sessionName, phase string) bool {
	evs, _, _, err := eventlog.NewStore(store.Dir()).List(sessionName, 0, event.Filter{
		Types: []string{event.TypeLifecyclePrefix + phase},
	})
	return err == nil && len(evs) > 0
}

// recordLifecycle appends a lifecycle.<phase> event for a session. It is
// best-effort: failing to record must never fail the lifecycle operation.
func recordLifecycle(store *state.Store, sessionName, phase, summary string) {
	if sessionName == "" {
		return
	}
	_, _, _, _ = eventlog.NewStore(store.Dir()).Append(event.Event{
		SessionName: sessionName,
		Type:        event.TypeLifecyclePrefix + phase,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
		Summary:     summary,
	})
}

// recordJudgeRecorded appends the plect.judge.recorded builtin trigger to the
// *target* session's own log (not the judge's) — RecordJudge already resolved
// the judged session as sessionName, and instance names the task instance the
// verdict was recorded against (the contract's DoneWhenJudge carries neither,
// since both are always the caller's own context rather than a duplicated
// record field). The tick reactor always ticks on this event regardless of
// any `[tick]` declaration; best-effort like recordLifecycle.
func recordJudgeRecorded(store *state.Store, sessionName, instance string, judge *contract.DoneWhenJudge) {
	if sessionName == "" || judge == nil {
		return
	}
	_, _, _, _ = eventlog.NewStore(store.Dir()).Append(event.Event{
		SessionName: sessionName,
		Type:        event.TypeJudgeRecorded,
		Source:      event.SourcePlect,
		Direction:   event.Internal,
		Summary:     fmt.Sprintf("judge %s recorded (%s) by %s", judge.LeafID, judge.Action, judge.JudgeSession),
		Body:        judge.Reason,
		Metadata: map[string]string{
			"instance":       instance,
			"leaf_id":        judge.LeafID,
			"action":         judge.Action,
			"revision":       judge.Revision,
			"judge_session":  judge.JudgeSession,
			"judge_workflow": judge.JudgeWorkflow,
			"relation":       judge.Relation,
		},
	})
}

func instructionOutput(outputs map[string]any) string {
	if s, ok := outputs["instruction"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// appendInstruction records a task's `instruction` output as a plect.instruction
// event so the session dispatcher's runtime channel delivers it — TaskSetup keeps
// producing outputs and never delivers to a runtime itself. Best-effort like
// recordLifecycle: the instance already succeeded, so a failed append must not
// unwind it. No-op when there is no instruction.
func appendInstruction(store *state.Store, sessionName, taskKey, resource, instruction string) {
	if sessionName == "" || instruction == "" {
		return
	}
	meta := map[string]string{"task": taskKey}
	if resource != "" {
		meta["resource"] = resource
	}
	_, _, _, _ = eventlog.NewStore(store.Dir()).Append(event.Event{
		SessionName: sessionName,
		Type:        event.TypeInstruction,
		Source:      event.SourcePlect,
		Direction:   event.Inbound,
		Summary:     fmt.Sprintf("%s instruction", taskKey),
		Body:        instruction,
		Metadata:    meta,
	})
}

// resolveSessionName maps an identifier (session name, alias, or resource
// id) to the canonical session name used as the event-log key. It mirrors the
// core resolveSession precedence — exact name, alias, then workspace
// provider resolver dispatch — so event commands resolve the same way as
// create/up/show. Unlike resolveSession it does NOT require the session to
// exist (destroyed sessions retain their log), so a non-matching identifier
// falls back to itself rather than erroring.
func resolveSessionName(cfg *config.Config, store *state.Store, identifier string) (string, error) {
	if session, err := store.GetE(identifier); err != nil {
		return "", err
	} else if session != nil {
		return identifier, nil
	}

	if hits, err := store.FindByAliasE(identifier); err != nil {
		return "", err
	} else if len(hits) == 1 {
		return hits[0].Name, nil
	} else if len(hits) > 1 {
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.Name
		}
		slices.Sort(names)
		return "", &Error{Code: ErrInvalidInput, Message: fmt.Sprintf("identifier %q matches multiple sessions (%s); use the session name", identifier, strings.Join(names, ", "))}
	}

	// Workspace provider resolver dispatch (pure/offline). cfg is nil in some unit tests.
	if cfg != nil {
		if disp, matched, err := dispatchResource(cfg, "", identifier); err == nil && matched {
			return disp.Name, nil
		}
	}

	return identifier, nil
}
