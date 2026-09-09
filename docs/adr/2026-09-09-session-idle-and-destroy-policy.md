# Session-level idle-down and destroy policy, replacing population auto_down/auto_destroy

## Context

A dispatched session (one created under a chain, or under a population) has
no idle-down mechanism of its own. `docs/language/workflows.md`'s Populations
and chains section already documents a capacity-pressure down/re-up path, but
it is gated on two population-only fields, `auto_down` and `auto_destroy`
(`app/internal/config/workflowdoc.go`), and only ever runs from
`app/internal/population/capacity.go`'s `idleCandidates`, which restricts its
scan to `session.Population != nil` members
(`logicalVirtualRootChild(session) && definition.Population.AutoDown`). A
session dispatched by a chain, or created by a bare `plect up`, carries no
`Population` field and is invisible to that scan regardless of how long it
sits idle.

The result was observed directly: an orchestrator left six chain-dispatched
children `up` for seven hours across 24 quiet reactor ticks, none of them
brought down. The prompt layer already instructs a dispatcher to down its
idle children, and that instruction was not followed — a policy stated only
in a prompt has no enforcement path, so it moves to the mechanism layer,
generalized to any session that declares it, not only population members.

Three existing pieces of mechanism are reused rather than replaced:

- The eligibility predicate `idleCandidates` already computes — clear status
  newer than creation, acceptance, and every inbound event — is exactly what
  a chain-dispatched session's idle-down check also needs; only its
  population-only gate needs to widen.
- `app/internal/population/engine.go`'s `decideDestroy` already guards
  destruction on `e.hooks.Blockers` (every dynamic task instance with a
  `done_when` must be satisfied) before consulting `AutoDestroy`. That guard
  is resource-shaped, not population-shaped, and generalizes unchanged.
- `app/internal/reactor/forward.go`'s `sessionForwarder`
  (`docs/adr/2026-09-08-down-session-resource-event-forwarding.md`) already
  relays an inbound event on a down session to its nearest live ancestor.
  Decision 3 below builds the down-session delivery rule on top of that
  existing single-hop relay rather than inventing a second one.

One open question needed a code answer, not a guess: does a down session's
`done_when` still evaluate, fire chains, and push terminals today?
`app/internal/reactor/supervisor.go`'s `reconcile` starts a `sessionReactor`
(the follower that calls `doTick`, which evaluates `done_when`, fires chains,
and pushes terminals) only while `cfg.RunScopeUp(s)` holds, and swaps it for
a `sessionForwarder` — which only relays resource events one hop, never
ticks — the moment the session goes down. So today, no: a down session's
`done_when` does not evaluate, its chains do not fire, and no terminal is
pushed, until an explicit `up` restarts its `sessionReactor`. Decision 3
states the target rule this ADR chooses instead.

`app/internal/service/event.go`'s `recordLifecycle` already appends
`lifecycle.down` and `lifecycle.destroyed` (via `event.TypeLifecyclePrefix +
phase`) on every `Down` and `Destroy` call, with no metadata beyond a fixed
summary string. Decision 4 adds a `reason` key to that existing call rather
than introducing a new event type.

## Decision

### 1. Workflow-level `[session]` policy

```toml
[claude.session]
idle_down_after = "30m"

[claude.session.destroy]
force  = false
inputs = { delete_branch = false }
all = [
  { check = "resource.state.issue_status", in = ["closed"] },
]
```

`idle_down_after` (duration, optional) makes a session eligible for an
automatic down: the reactor brings a session down through ordinary cleanup
once its latest durable status is an explicit clear newer than its creation,
its most recent accepted appearance, and every inbound event, and it has
stayed so for at least the declared duration. Absent, a session is never
downed automatically. A session with no real parent (`ParentSession == ""` —
an operator's own directly-created session, not one a chain or population
dispatched) is never eligible, regardless of the declaration: idle-down
exists for dispatched work, not for a session a person is using directly.

The same declaration is also the sole authorization for capacity-pressure
down (decision 2); there is no separate boolean for that path. A duration
rather than a boolean because a dispatched session's trigger is elapsed quiet
time, not capacity pressure, and because the duration doubles as the debounce
that prevents an immediate up/down bounce around a re-evaluate kick.

`[<workflow>.session.destroy]` moves the population fields `force` and
`inputs` here, unchanged in meaning (whether destruction uses `--force`, and
the plugin-owned cleanup input object). `destroy.all` (optional) is a
conjunction of `done_when` leaves — `check`/`in` and `expr`, never `judge`
(`docs/language/tasks.md`'s completion grammar) — evaluated over the entry
resource's `resource.state.*` only, since there is no task instance and so no
`self.state.*` to read. Reusing `done_when`'s own grammar and evaluator means
a destroy policy is not a second expression language a reader has to learn.
The guard is unchanged from populations: destruction waits until every
dynamic task instance with a `done_when` is satisfied first; a missing
predicate, an observation failure, an evaluation failure, or any pending leaf
blocks it, `destroy.all` included. A population's absence tombstone (poll
absence past `expire_after`) remains a built-in additional destroy trigger
that flows through that same guard — it is a second way to become eligible,
not a second guard.

There is deliberately no per-session pin/keep flag: a session that must stay
up simply omits `idle_down_after`, and one instruction is enough — a second,
independent escape hatch would let an operator suppress the reactor's
decision without changing the declaration that produced it.

### 2. Capacity-pressure down is symmetric across both capacity keys

`config.toml`'s machine-wide `max_up_children` and a workflow's real-children
`max_up_children` (`docs/language/config.md`, `docs/language/workflows.md`'s
Concurrency section) become one rule instead of two: whenever any admission —
population, manual `plect up`, or a chain's dispatched `plect up` — hits
either key, the reactor selects, within that key's own scope (the virtual-root
cohort for the machine-wide key, or the same parent's children for a
workflow's real-children key), from sessions that declare `idle_down_after`
and are currently clear by the decision-1 predicate, ordered oldest activity
then session name, downs the first one, and admits. No eligible candidate
still rejects the admission, exactly as today. `idle_down_after` is the sole
authorization for this selection — declaring it opts a session into
capacity-pressure down, replacing `auto_down`'s narrower, population-only
grant. `docs/language/config.md`'s sentence that "a manual up rejected at the
cap does not authorize the resident evaluator to bring another session down"
no longer holds and is removed: the declaration is the authorization,
independent of who requested the admission that hit the cap. Capacity
pressure exists only where a `max_up_children` is actually set; an unset cap
leaves admission unlimited and this mechanism dormant.

### 3. Delivery to a down session

Core owns exactly two directed event types, `user.emit` and
`plect.instruction`: delivery of either to a down session brings it up first
(an ordinary `up`, through the same path a manual re-up already takes), then
delivers. Every other type is a notification: it is forwarded to the nearest
live ancestor by the existing `sessionForwarder` rule
(`docs/adr/2026-09-08-down-session-resource-event-forwarding.md`), and when no
live ancestor exists it simply stays on the down session's own log, unbrought
up, to be read once that session resumes. Core does not learn plugin-defined
types to special-case: anything core does not itself own as directed is a
notification, full stop — a plugin type never brings a session up on its own.

The target rule for the down-session tick gap identified in Context: the tick
reactor's own evaluation — `done_when`, chain firing, terminal pushes — runs
regardless of run state; a session's own quiet-tick backoff and heartbeat
schedule keep operating while it is down exactly as while it is up. Only a
pane-dependent kick (an interactive attach, a directed delivery under this
decision) brings the session's run state back up; ticking itself no longer
implies bringing anything up. This closes the gap decision 1's
`idle_down_after` would otherwise open: a session downed for being idle must
still reach its own destroy guard and its own terminal signals without
requiring a human or a directed event to resurrect it first.

### 4. Observability

A capacity-pressure down prints one line on the standard output of the
`plect up` invocation that triggered it: which session it brought down, how
long it had been idle, which capacity key was under pressure, and which
admission the down freed capacity for.

`lifecycle.down` and `lifecycle.destroyed` (`app/internal/service/event.go`'s
`recordLifecycle`, `event.TypeLifecyclePrefix + phase`) gain a
`metadata.reason` field: `idle` or `capacity` for a down, `policy` or
`absence` for a destroy. A manual `plect down`/`plect destroy` carries no
`reason` — the field only ever names an automatic trigger, so its absence is
itself informative. No new event type: the existing two lifecycle events are
the whole vocabulary: `plect ls` reads this same `reason` to display it next
to a down session.

### 5. Removals

The population fields `auto_down` and `auto_destroy`, `session.destroy.force`
and `session.destroy.inputs` under `[[<workflow>.populations]]` (moved to the
workflow-level `[session]`/`[session.destroy]` tables above), the
`plect.workflow_population.destroy` event (superseded by `lifecycle.destroyed`
carrying `metadata.reason = "policy"` or `"absence"`), and the "no policy →
dry run recorded" behavior (`plect.workflow_population.destroy_dry_run`,
produced today when `AutoDestroy` is false) are removed outright. None of
these are used by the only live deployment (`docs/naming.md`'s companion
devbox configuration), and Plecture is pre-1.0
(`CLAUDE.md`'s Compatibility policy): a removal ships without a compatibility
shim.

### Out of scope, follow-up issues

- The Slack plugin's `slack_subscribe` binding becomes session-scoped and
  drops its `socket_path` dependency, so a mention on a down session's thread
  reaches it as a `user.emit` under decision 3's directed-delivery rule.
- The GitHub plugin's `pull_request` state schema gains a `state` property
  (`open` | `closed` | `merged`), which a `destroy.all` leaf like
  `{ check = "resource.state.state", in = ["closed", "merged"] }` can read.
- The devbox configuration declares `idle_down_after` and
  `[session.destroy]` on its claude/codex/orchestrator workflows.

## Consequences

- A chain-dispatched or manually-created session becomes eligible for
  automatic down and destroy by declaring `[session]` policy on its
  workflow, independent of whether it is ever a population member.
- The machine-wide and per-workflow capacity keys share one down-selection
  rule instead of the population-only one `docs/language/config.md` and
  `docs/language/workflows.md` describe today; a manual or chain-dispatched
  admission can now free capacity by bringing down an idle-eligible sibling,
  where before only a population admission could.
- A session's tick no longer implies its run state: `done_when` evaluation,
  chain firing, and terminal pushes continue while a session is down, so an
  idle-down session still reaches its own destroy guard without a directed
  event or manual `up` resurrecting it first.
- `destroy.all` reuses the `done_when` leaf grammar and evaluator
  (`docs/language/tasks.md`), so a destroy policy is read, validated, and
  diagnosed exactly like a task's completion predicate — no second
  expression surface for a config author to learn.
- Three population events retire (`plect.workflow_population.destroy`,
  the implicit dry-run behavior, and the population-only `auto_down`/
  `auto_destroy` fields); anything reading them for the removed behavior
  must move to `lifecycle.down`/`lifecycle.destroyed` with `metadata.reason`.
- The follow-up issues (Slack `slack_subscribe` scoping, GitHub
  `pull_request.state`, devbox policy declarations) remain unimplemented
  until filed and merged separately; this ADR authorizes their shape but not
  their code.

## Alternatives considered

- **A separate boolean for capacity-pressure eligibility**, keeping
  `idle_down_after` for time-based down only. Rejected: the two triggers
  (elapsed idle time, capacity pressure) both answer "is this session safe to
  bring down right now," and a session that is safe to down for one reason is
  safe to down for the other — a second flag would only let a config author
  reach an incoherent state (eligible for one trigger but not the other) with
  no legitimate use for it.
- **A per-session pin/keep flag** as an escape hatch alongside
  `idle_down_after`. Rejected: omitting the duration already means "never,"
  so a second, independent way to say the same thing would let an operator's
  override silently diverge from the declaration meant to describe policy,
  with no way to tell from the declaration alone which one is actually in
  effect.
- **A second expression syntax for `destroy.all`**, purpose-built for
  resource-only predicates instead of reusing `done_when`'s leaf grammar.
  Rejected: the predicate shape (a conjunction of key comparisons and
  expressions over a schema-declared root) is identical to what `done_when`
  already validates, evaluates, and diagnoses; a second syntax would only
  duplicate that machinery for no behavioral gain.
- **Keeping the down-session tick gap and instead special-casing
  `idle_down_after` sessions to always evaluate `done_when` even while
  down**, rather than making that the general rule for every session.
  Rejected: a carve-out for one policy would leave every other down session's
  `done_when`, chains, and terminals silently inert until `up`, which is the
  same bug this ADR exists to close, merely narrowed to sessions without
  `idle_down_after`.
- **Bringing a session up on every forwarded notification**, so the target
  rule for decision 3 needs no distinction between directed and notification
  types. Rejected: `docs/adr/2026-09-08-down-session-resource-event-forwarding.md`
  already chose the opposite for the identical question (waking the origin
  undoes the `down` an operator or a chain's backoff policy chose) for the
  same reason forwarding exists in the first place — keeping a session down
  and still reactive. This decision only extends that existing choice to
  `user.emit`/`plect.instruction` as the two exceptions that must still wake
  their target.
