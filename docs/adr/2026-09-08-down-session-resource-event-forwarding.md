# Route a down session's inbound events to its nearest live ancestor

## Context

A session's reactor only runs while its run scope is up
(`reactor.Supervisor.reconcile` starts a `sessionReactor` goroutine per up
session and cancels it the moment the session goes down). A resource an
`up` session registered — a pull request, an issue, any external thing a
workspace-provider plugin watches — keeps emitting events for as long as the
plugin's own watcher is subscribed, independent of whether the session that
registered it is still up. Once that session goes `down` (but is not
destroyed), nothing drains its event log any more: an inbound event for its
resource lands and sits there unreacted until the session is brought back
up, however long that takes.

Two existing single-hop cross-session signals already solve a structurally
identical problem for a different trigger: `service.CheckHeartbeatDeadman`
and `service.HealthcheckSession`'s escalation path both resolve the nearest
live ancestor via `resolveLiveAncestor` (walking `ParentSession`, skipping
any ancestor whose own health reads unhealthy or stalled) and push a
`plect.terminal.*` event one hop into that ancestor's own log via
`publishTerminalTo`. Neither is reused as-is for an ordinary resource event,
because a terminal push means "origin has reached some final state, decide
what to do about it"; a resource event on a still-live-but-down session
carries no such verdict — the origin is not finished, and its own tick
resumes handling its log the moment it comes back up.

Absent a mechanism for this, a dispatcher's only options are to
`plect subscribe` the resource itself (duplicating the child's own
subscription) or keep the child `up` purely to stay reactive, defeating the
quiet-tick backoff a heartbeat-scheduled workflow otherwise gets.

## Decision

Introduce a third per-session follower, `sessionForwarder`
(`app/internal/reactor/forward.go`), alongside the existing pair
(`sessionReactor` for tick, `sessionDispatcher` for channel delivery). The
supervisor starts one for every down-but-not-destroyed session and stops it
the instant that session is destroyed (state row gone) or comes back up (a
`sessionReactor` takes over instead) — `Supervisor.reconcile` gates the two
followers on exact complements of `RunScopeUp`, so a session never runs both
at once.

A `sessionForwarder` follows its own session's log exactly like the other
two followers (`sessionhub.Registry.Watch` wake, a durable per-consumer
cursor seeded at the tail on first start so pre-existing history is never
replayed as a fresh forward, one cursor commit per relayed event so a crash
mid-batch replays at most one event on restart). For every new `Inbound`
event, it calls `service.ForwardDownSessionEvent`, which resolves the
nearest live ancestor via the same `resolveLiveAncestor` the terminal/
deadman pushes already use, and — when one exists — relays the event one hop
into that ancestor's log via `publishTerminalTo`, as a new event type,
`plect.resource.forwarded` (carrying the origin session name via the
existing `MetaOriginSession` convention, the origin's registered
`ResourceID`, and the original event's type/summary/metadata url). The push
is deduplicated on the original event's own id, so a retried relay after a
crash never double-pushes. Waking is one-directional: the ancestor is woken
if down (matching every other cross-session push in this codebase), but the
down origin itself is never woken by its own forwarding — only an explicit
`up` resumes its own tick.

The ancestor's `sessionReactor` treats a received `plect.resource.forwarded`
as an unconditional tick trigger, alongside the existing judge-verdict
builtin (`plect.judge.recorded`) — checked ahead of the self-emitted
exclusion and the declared `[tick].on` pattern match, since a workflow has no
way to declare a pattern in advance for a signal it never subscribed to
itself.

When `resolveLiveAncestor` finds nothing (a root session, or a chain that is
itself entirely down/unhealthy), `ForwardDownSessionEvent` returns cleanly
without pushing anything — unlike the deadman path's fallback of recording
an "undeliverable" event on the origin itself. A resource event with nowhere
live to go is not itself bad news requiring a durable record; it is simply
today's existing behavior (the event lands on the origin's own log, to be
read once the origin resumes).

## Consequences

- A new eventlog cursor consumer kind (`resourceforward`) required widening
  `event_cursors.kind`'s CHECK constraint, hence a schema migration
  (`persistence/migrations`) alongside the `schema.sql` change.
- Forwarding latency is bounded by the same wake mechanism the reactor and
  dispatcher already rely on (`sessionhub.Registry`), not by a fixed polling
  interval — a down child's resource event reaches the live ancestor's own
  log, and that ancestor's already-running reactor ticks off it, within the
  same sub-second latency an `up` session's own reactive tick already gets.
- A workspace-provider plugin's choice to keep emitting events for a
  finished resource (a merged PR, a closed issue) is unaffected and out of
  scope: core relays whatever Inbound events arrive without interpreting
  what they mean.

## Alternatives considered

- **A periodic supervisor-level sweep** (mirroring `checkDeadman`'s own
  fixed-interval scan of up sessions) instead of a third per-session
  follower. Rejected: a sweep's polling interval directly bounds forwarding
  latency, and there is no single interval that is both cheap enough to run
  often and fast enough to matter for a human review arriving on a down
  child's PR. Reusing the existing per-session wake infrastructure gives
  near-real-time forwarding without inventing a new latency/cost tradeoff.
- **Letting the ancestor's declared `[tick].on` pattern govern whether a
  forwarded event triggers a tick**, treating it like any other inbound
  type a workflow opts into. Rejected: the forwarded event's type is a core
  mechanism the ancestor's workflow author never declared and has no reason
  to know about in advance; requiring an opt-in pattern would silently drop
  every forward whose target workflow doesn't happen to declare a matching
  `on` pattern.
- **Waking the down origin itself as part of forwarding**, so it resumes
  ticking immediately once its resource is touched. Rejected: forwarding
  exists precisely so a session can stay down (and keep its quiet-tick
  backoff) while still being reactive through its ancestor; waking it on
  every forwarded event would silently undo the `down` an operator (or a
  chain's own backoff policy) chose.
