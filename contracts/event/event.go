// Package event defines the shared contract types for the plect event bus:
// a per-session, append-only, durable pub/sub log.
//
// The bus core (eventlog, bus server) treats SessionName and Type as opaque
// strings — it does not interpret provider-specific structure (a resource
// provider's identifier shape, a chat provider's thread ids, etc.).
// Producers own their Type namespace (a chat provider's message type, an
// agent's reply type, a resource provider's own change-type prefix, ...);
// the core only routes and filters. Provider-specific Source/Type constants
// belong in that provider's own package, not here.
//
// This module is zero-dependency (stdlib only) so app and plugins
// can all import it, mirroring contracts/{state,hook,channel-protocol}.
package event

import (
	"slices"
	"strings"
	"time"
)

// Direction is the flow of an event relative to the plect session.
type Direction string

const (
	Inbound  Direction = "inbound"  // toward the agent (e.g. a chat user's message)
	Outbound Direction = "outbound" // away from the agent (e.g. an agent reply, a chat post)
	Internal Direction = "internal" // neither in nor out (e.g. a resource-provider sync change, lifecycle)
)

// Source identifies who produced the event. These are conventions for
// producers; the core does not interpret them. A provider-specific source
// (a chat platform, an agent) is that provider's own constant, not one
// listed here.
const (
	SourcePlect = "plect"
	SourceWeb   = "web"
	SourceCLI   = "cli"
	SourceMCP   = "mcp"
	// SourceTick marks every same-session event plect tick itself publishes
	// (review_required, kick's user.emit, escalated). The tick reactor
	// excludes anything carrying this source from its trigger set by
	// provenance rather than by enumerating types, so a kick's user.emit —
	// which otherwise looks like an ordinary user.emit — cannot retrigger
	// tick under a broad declared pattern (e.g. "*" or "user.emit").
	SourceTick = "plect.tick"
)

// Type prefixes / well-known types. Type is a free-form dotted topic; these are
// the namespaces plect itself produces. Subscribers filter with globs (e.g.
// a resource provider's own change-type prefix, or a chat provider's own
// message-type prefix).
const (
	TypeLifecyclePrefix = "lifecycle." // lifecycle.created|up|down|destroyed
	TypePermissionReply = "permission.reply"
	TypeUserNote        = "user.note"
	TypeUserEmit        = "user.emit"
	// TypeInstruction is a task instruction appended to a session's stream for
	// delivery to its runtime via a workflow channel (not sent from TaskSetup).
	TypeInstruction = "plect.instruction"
	// TypeChannelError records a channel worker exhausting its retries. It rides
	// the log for observability but is never itself a channel `include` target,
	// so a failed delivery cannot loop.
	TypeChannelError = "plect.channel.error"
	// TypeStatusMessage records a session's self-reported status line whenever
	// it changes.
	TypeStatusMessage = "plect.status_message"
	// TypeTerminalPrefix namespaces the cross-session terminal signals defined
	// by the terminal-event-propagation ADR: done, escalate, dead. A terminal
	// event is pushed one hop into the *receiving* session's own log (D1-D3),
	// so its SessionName is the receiver, not the emitter — MetaOriginSession
	// names the emitter.
	TypeTerminalPrefix   = "plect.terminal."
	TypeTerminalDone     = "plect.terminal.done"
	TypeTerminalEscalate = "plect.terminal.escalate"
	TypeTerminalDead     = "plect.terminal.dead"
	// TypeResourceForwarded is pushed one hop into the nearest live ancestor's
	// log (SessionName is the receiver, MetaOriginSession the emitter, like the
	// terminal signals above) when an inbound event lands on a session that is
	// down (not destroyed): no per-session reactor drains a down session's own
	// log, so without this push such an event would sit unreacted until the
	// session is brought back up. It is not itself a terminal signal — the
	// origin session is not finished, and its own tick resumes handling its
	// log the moment it comes back up.
	TypeResourceForwarded = "plect.resource.forwarded"
	// TypeTickReviewRequired and TypeTickEscalated are plect tick's own
	// same-session progress markers (internal/service/tick.go). Neither is a
	// terminal event nor an external-resource signal; both are excluded from
	// the tick reactor's trigger set so a declared `[tick].on` pattern broad
	// enough to match them (e.g. "*") cannot make tick re-trigger itself.
	TypeTickReviewRequired = "plect.tick.review_required"
	TypeTickEscalated      = "plect.tick.escalated"
	// TypeJudgeRecorded is a same-session builtin signal appended to the
	// *target* work session's log (not the reviewer's) whenever a judge
	// verdict is recorded, independent of any `[tick]` declaration — the tick
	// reactor always reacts to it by ticking that target session.
	TypeJudgeRecorded = "plect.judge.recorded"
	// TypeChainAttempt records a [[chains]] spawn attempt that fired but
	// created no session — today the only producer is a parent's
	// max_up_children cap refusal (Metadata["reason"] = "cap"). It is
	// appended to the *ticking* session's own log, not the derived target's
	// (which does not exist), so a dispatcher walking the subtree sees the
	// refusal without re-running `plect status`. A tick dedupes on
	// (chain_id, instance, target, reason) so a refusal streak records once,
	// not once per tick.
	TypeChainAttempt              = "plect.chain.attempt"
	TypeWorkflowPopulationDestroy = "plect.workflow_population.destroy"
	TypeWorkflowPopulationDown    = "plect.workflow_population.down"
	// TypeWorkflowPopulationUp means the member's session just transitioned
	// to up. Re-admitting a member that was already up records nothing: the
	// admission path re-runs its idempotent up hook on any inbound signal, so
	// an unconditional record would report presence changes that never
	// happened.
	TypeWorkflowPopulationUp              = "plect.workflow_population.up"
	TypeWorkflowPopulationConflict        = "plect.workflow_population.conflict"
	TypeWorkflowPopulationFailure         = "plect.workflow_population.failure"
	TypeWorkflowPopulationDestroyDeferred = "plect.workflow_population.destroy_deferred"
	TypeWorkflowPopulationDestroyDryRun   = "plect.workflow_population.destroy_dry_run"
	// TypeNodeResult records a workflow node's setup, cleanup, or liveness
	// verification completing, or being skipped after a successful liveness
	// check. It fires the same way for a manual session, a child session, and
	// a population-produced member — appended to that session's own log,
	// independent of TypeWorkflowPopulationUp/Down. Deduplicated only by the
	// log's ordinary append identity: a repeated attempt is a separate fact,
	// not a dedup collision.
	TypeNodeResult = "plect.node.result"
	// TypeMessage and TypeMessageChunk are the runtime-neutral agent-message
	// contract documented at docs/language/events.md#agent-messages: for one
	// MetaMessageID, an emitter produces either exactly one TypeMessage or a
	// TypeMessageChunk sequence ending with MetaFinal = "true", never both.
	TypeMessage      = "plect.message"
	TypeMessageChunk = "plect.message_chunk"
)

// Metadata keys stamped on TypeMessage / TypeMessageChunk events; see
// docs/language/events.md#agent-messages for the full contract.
const (
	MetaMessageID = "message_id"
	MetaTurnID    = "turn_id"
	MetaRole      = "role"
	MetaSource    = "source"
	MetaIndex     = "index"
	// MetaFinal is the literal string "true" or "false" (Metadata is
	// map[string]string).
	MetaFinal = "final"
)

// RoleAssistant is TypeMessage's MetaRole value.
const RoleAssistant = "assistant"

// NodeResultAction is TypeNodeResult's closed set of "action" metadata
// values: which lifecycle action the event reports on.
const (
	NodeResultActionSetup   = "setup"
	NodeResultActionCleanup = "cleanup"
	NodeResultActionAlive   = "alive"
)

// NodeResultOutcome is TypeNodeResult's closed set of "result" metadata
// values: what that action produced.
const (
	NodeResultProduced = "produced"
	NodeResultSkipped  = "skipped"
	NodeResultFailed   = "failed"
	NodeResultCleaned  = "cleaned"
)

// Metadata keys stamped on a pushed terminal event (TypeTerminalDone /
// TypeTerminalEscalate / TypeTerminalDead).
const (
	// MetaOriginSession names the session the terminal fact is about (the
	// event's own SessionName is the receiving parent/ancestor instead).
	MetaOriginSession = "origin_session"
	// MetaRelation is the receiver's tree relation to the origin, stamped at
	// push time (mirrors DoneWhenJudge.Relation's record-time-fact pattern).
	MetaRelation = "relation"
	// MetaDedupKey is the idempotency key a repeated push checks against the
	// target's recent terminal events before appending (P1).
	MetaDedupKey = "dedup_key"
	// MetaInstance names the task instance a done/escalate push is about.
	MetaInstance = "instance"
)

// Event is both the durable log record and the pub/sub message. The replay
// cursor is a per-stream sequence number, carried out-of-band (SSE id frame /
// List offsets) — never a field here. ID is the identity/dedup key, not the
// cursor.
type Event struct {
	ID          string            `json:"id"`           // ULID: global uniqueness + dedup
	SessionName string            `json:"session_name"` // opaque session id; the log partition + routing key
	Time        time.Time         `json:"time"`         // RFC3339Nano
	Type        string            `json:"type"`         // free-form dotted topic
	Source      string            `json:"source"`       // plect|web|cli|mcp|<provider>
	Direction   Direction         `json:"direction"`
	Summary     string            `json:"summary"`        // one-line render for timelines
	Body        string            `json:"body,omitempty"` // full text payload
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Filter selects events for listing or subscription. A zero Filter matches
// everything (Limit is applied by the caller, not by Match).
type Filter struct {
	Types     []string  // glob patterns; empty = any
	Sources   []string  // exact match; empty = any
	Direction Direction // exact; empty = any
	Limit     int       // 0 = no limit (caller-applied)
}

// Match reports whether ev satisfies the filter's Types/Sources/Direction.
func (f Filter) Match(ev Event) bool {
	if len(f.Types) > 0 {
		ok := false
		for _, p := range f.Types {
			if MatchType(p, ev.Type) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(f.Sources) > 0 && !slices.Contains(f.Sources, ev.Source) {
		return false
	}
	if f.Direction != "" && ev.Direction != f.Direction {
		return false
	}
	return true
}

// MatchType reports whether typ matches pattern. Pattern is either "*" (any),
// an exact type, or a trailing ".*" prefix glob ("foo.*" matches "foo.bar"
// and "foo." but not "foo").
func MatchType(pattern, typ string) bool {
	if pattern == "*" || pattern == typ {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(typ, pattern[:len(pattern)-1])
	}
	return false
}

// SplitCSV splits a comma-separated Filter.Types/Sources argument into
// trimmed, non-empty elements, so CLI, MCP, and the bus HTTP face agree on
// how "a, b" and " a,,b " parse. A blank input returns nil, matching
// Filter's "empty = any" convention.
func SplitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
