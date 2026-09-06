# Runtime failure model

## Context

Plecture state records lifecycle production: an effect instance is
`produced`, `failed`, or `cleaned`, and a session's run state is derived from
whether any run-scoped instance is produced. Effect outputs are production
records, not live facts. Health probes observe some live facts, but an effect
without a probe contributes nothing to health.

That division is correct, but it leaves the lifecycle model without an
authority for reuse. A produced instance can hold outputs that no longer name
anything usable, and `plect up` can skip it because the production record is
still present. A partial setup failure can leave the run reading `up` when one
early node produced and a later required node failed or was never attempted.
The CLI reports that failure to the foreground caller, but the session log does
not contain a node-level lifecycle result that a workflow channel can relay.

The issue reports this through several concrete failures:

- A runtime, credential guard, subscription, and terminal pane disappear across
  a container replacement while produced records remain (#368).
- A runtime launch failure leaves later nodes unattempted, while the coarse run
  state can still read `up` because an earlier run-scoped node produced
  (#364, #370).
- A channel-side status can imply progress even when no runtime handoff occurs
  (#363).
- A workflow population `up` event is intentionally gated on a real not-up to
  up transition, so an in-place repair cannot rely on that event to reach a
  session's conversation channel (#394).

The decisions here keep output records as records of production. They make
liveness the authority for reusing those records, extend health to name an
incomplete run, and record what happened to each node attempt.

## Decision

### 1. Liveness verification and verify-before-skip

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected. Operators can add recovery instructions, but a prompt cannot make `plect up` distinguish reusable outputs from stale outputs. |
| Require deployments to down then up before every resume | prompt | Rejected. It makes every caller duplicate lifecycle policy and still gives no per-node reason in state or events. |
| Add a second validity probe | config, plugin, core | Rejected. No shipped setup-bearing effect has a reuse condition distinct from liveness, so a second executable surface would be speculative. |
| Use `[health].alive` as the skip authority | config, plugin, core | Recommended. The existing liveness probe answers whether a produced surface still exists; core runs it at the explicit reuse decision. |

#### Recommendation

`[health].alive` is the executable authority for reusing a produced effect.
It already observes the effect's own outputs, resolved inputs, session, and
workspace, and exit zero already means that its owned surface is present. At
`plect up`, core runs that action for every produced node, including a
session-scoped node. A non-zero exit, an unresolved required value, timeout,
or invalid action configuration means that node cannot be reused.

Plan health has a terminal evaluation boundary. While `plect up` is walking a
plan, it does not evaluate nodes that the walk has not reached as missing:
those nodes are pending setup, not unhealthy. During that interval status keeps
the last completed health result and the health cycle neither publishes a new
verdict nor escalates it. Core evaluates the frozen plan only after the `up`
attempt returns.

After a successful `up`, every planned node has completed setup and health
evaluates its produced entries. After an aborted `up`, the failed entry and
every unattempted current-plan entry are structural failures, so the completed
attempt reports `unhealthy` with the first such node named. `run` remains
binary: an aborted attempt with a produced run-scoped prefix reads `up` with
`unhealthy` health; one that produces no run-scoped node reads `down` with
`unhealthy` health in status. The periodic health cycle remains gated on
`run = up`, so the latter has no health escalation until a later successful
`up` produces a run surface.

For a completed run that is up, the ordinary health cycle executes liveness
probes for produced nodes of either scope and inspects every current-plan task
record for failed or missing nodes. It reports a liveness or structural failure
but never repairs it automatically. Repair occurs only during an explicit
`plect up`; this avoids making a periodic observation cycle mutate external
resources or retry a provider action without operator or workflow intent.

The migration adds `[health].alive` to `gh_app_guard`, `slack_subscribe`,
`gh_guard`, and `worktree`. Once their session is up, each participates in the
same plan health evaluation even where its own scope is `session`. Losing one
therefore produces the ordinary `health.unhealthy` escalation to the nearest
live parent, with the failing effect named. The next `plect up` observes the
failed liveness check, reconstructs the affected node and its dependents, and
clears the condition after the rebuilt plan passes health. A health sweep only
reports and escalates; it never runs cleanup or setup to repair the effect.

```toml
[gh_app_guard]
kind  = "effect"
scope = "run"

[gh_app_guard.setup]
type = "exec"
bin  = "gh-app-guard"
args = ["setup"]

[gh_app_guard.cleanup]
type = "exec"
bin  = "gh-app-guard"
args = ["cleanup", "--dir", { from = "self.outputs.dir" }]

[gh_app_guard.health.alive]
type = "shell"
script = 'test -x "$dir/gh"'

[gh_app_guard.health.alive.bind]
dir = { from = "self.outputs.dir" }

[gh_app_guard.outputs_schema]
type     = "object"
required = ["dir"]

[gh_app_guard.outputs_schema.properties]
dir = { type = "string" }
```

`plect up` verifies before it skips a produced workflow node. The walk remains
dependency ordered:

1. A produced node with `[health].alive` is skipped only when that action
   succeeds.
2. A produced node with `reuse = "record"` is skipped from its record.
3. A produced node whose liveness action fails is marked failed with the
   liveness error, then the node and its produced dependents are cleaned in
   reverse dependency order using their stored outputs.
4. Setup resumes from the first invalidated node in dependency order.

`reuse = "record"` means Plecture deliberately does not ask whether the
produced entity still persists. It does not mean that the effect has no
external entity: a Slack thread is record-only because recreating a deleted
thread would split its conversation. The marker makes the record, rather than
an existence probe, the skip authority for that effect.

This follows [systemd's `RemainAfterExit=`](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#RemainAfterExit=),
where configuration declares that service state is retained after the process
that established it has exited. Plecture borrows only the explicit declaration
that it must not infer present external state from a past action; it does not
adopt systemd's service-state model.

At load time, every setup-bearing effect declares exactly one of
`[health].alive` and `reuse = "record"`. `reuse` accepts only the string
`"record"`. On an effect with no `setup`, `reuse` is a load error. These rules
apply independently to every layer of a nesting chain. A layer's existing
`[health].alive` composes by AND with the other layers' liveness probes, while
a `reuse = "record"` layer contributes no probe and does not suppress an inner
layer's liveness check.

```toml
[write_instruction]
kind     = "effect"
scope    = "session"
reuse    = "record"

[write_instruction.setup]
type = "shell"
script = 'printf %s "$instruction"'
```

This is a config, plugin, and core decision:

- Prompt cannot enforce it because a human instruction cannot change
  lifecycle skip semantics.
- Config declares the existing probe or the record-only exception but cannot
  decide when to run it.
- Plugin code owns liveness checks such as testing a socket, process, generated
  wrapper, or subscription registration.
- Core owns the only safe place to compare the stored production record with
  the liveness result and choose skip, cleanup, or setup.

This does not add a second configuration-language action. A future effect with
a concrete reuse condition stricter than liveness is the evidence required to
consider one.

### 2. Complete-plan health and binary run state

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected. Dispatchers would keep reconstructing partial failure by comparing task maps against workflow plans. |
| Add `degraded` to the run state | core | Rejected. It changes a binary capacity signal into a tri-state value and makes every current run consumer decide whether `degraded` is up-like. |
| Compose health over the completed current plan | core | Recommended. Health can name a failed or missing node, while `run` keeps its capacity meaning. |

#### Recommendation

`domain.RunState` remains binary and derived from produced run-scoped nodes:
`up` means that at least one current-plan run-scoped node is produced, and
`down` means none is. A workflow with no current-plan run-scoped nodes is
`down`. This keeps `run` as the capacity and cleanup signal consumed by child
capacity, population presence, and `plect down`.

Health composes over every current-plan node, not only the produced subset or
the run-scoped subset. A produced node evaluates its declared `alive` probe in
the ordinary way. A failed or missing node makes the session `unhealthy`
directly, with a reason naming the node and its failed dependency or setup
error. That rule also covers a failed `reuse = "record"` node, which has no
existence probe to run. Activity continues to compose from produced run-scoped
instances only.

A stale task entry for a node no longer in the workflow contributes to neither
run nor health; stale-node cleanup owns that lifecycle. A failed or missing
session-scoped node changes the completed plan health report, but not the
binary run state.

`plect ls --json` keeps its existing binary `run` value and reports the
incomplete plan through `health`; the health report's `Reason` names the failed
or missing node. The human table likewise reports `up` with `unhealthy` when a
produced prefix is missing a required node of either scope.

```json
[
  {
    "session_name": "kecbigmt/plecture-371+review_agent",
    "run": "up",
    "health": "unhealthy",
    "display_status": "review",
    "resource_id": "https://github.com/kecbigmt/plecture/issues/371",
    "tracked": true
  }
]
```

This is a core decision. Prompt and plugin code cannot publish a coherent run
state for every workflow because only core has the frozen workflow, current
plan, persisted task map, and state-listing API in one place. Config cannot
absorb it without making every workflow restate the same completeness rule.

No state-file migration is required. Health reads the existing task records and
the current plan, while `run` retains its existing two values.

### 3. Node-result lifecycle events

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected. Foreground stdout is not observable by a waiting conversation or supervisor after the command exits. |
| Reuse `plect.workflow_population.failure` | core | Rejected. Node results are session lifecycle facts; they occur for manual sessions and child sessions as well as population members. |
| Emit one session event per node attempt | core | Recommended. The event log is the existing durable delivery surface for workflow channels and observers. |

#### Recommendation

Core emits `plect.node.result` to the affected session log whenever a workflow
node's setup, cleanup, or liveness verification completes or is skipped after
a successful liveness check. The event is internal, sourced from `plect`, and
deduplicated only by the event log's ordinary append identity; repeated
attempts are separate facts.

```json
{
  "type": "plect.node.result",
  "source": "plect",
  "direction": "internal",
  "summary": "agent setup failed",
  "metadata": {
    "node": "agent",
    "effect": "official.claude.runtime",
    "scope": "run",
    "action": "setup",
    "result": "failed",
    "duration_ms": "120018"
  },
  "body": "claude not detected within 120s for session_id 5ec4..."
}
```

The closed `metadata.result` values are `produced`, `skipped`, `failed`, and
`cleaned`. The `body` carries a bounded stderr or error tail for failures. A
successful setup event does not duplicate full outputs; state remains the
authority for outputs.

Workflow event channels receive node-result events through the ordinary
`include` list:

```toml
[[review_agent.event.channel]]
name    = "conversation"
uses    = "official.slack.slack"
include = ["plect.node.result", "plect.status_message"]

[review_agent.event.channel.inputs]
base_url   = { from = "session.inputs.slack_base_url" }
channel_id = { from = "nodes.slack_thread.outputs.channel_id" }
thread_ts  = { from = "nodes.slack_thread.outputs.thread_ts" }
```

This is a core decision. Plugins can write their own logs, but only core sees
every lifecycle action and can name the workflow node consistently. Config can
select delivery with `include`, but it cannot manufacture missing lifecycle
events. Prompt text cannot reach unattended dispatchers or session channels
reliably.

### 4. Cleanup-on-failure and launch timeouts

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected for launch timeouts that already have observed slow-start consumers; viable only for effects that never start external work before success. |
| Make core kill launched processes generically | core | Rejected. Core has no provider-neutral identity for a process, socket registration, temporary credential cache, or external subscription before setup produces outputs. |
| Require effect setup to be failure-atomic and expose timeout inputs where latency varies | plugin, config | Recommended. The plugin that launches a surface is the only layer that knows how to abandon it safely. |

#### Recommendation

An effect setup that can start external work before it can produce its durable
outputs is failure-atomic by contract: when setup exits non-zero, it has
already stopped or orphan-proofed anything it started that would collide with a
retry. Core still records the failed node, emits `plect.node.result`, and
continues to run cleanup for already-produced dependencies. It does not invent
provider-specific cleanup for a setup action that produced no outputs.

Variable launch detection windows are effect inputs with defaults in the
plugin's `inputs_schema`, wired by workflow node inputs when a deployment needs
to tune them.

```toml
[runtime.inputs_schema]
type                 = "object"
additionalProperties = false

[runtime.inputs_schema.properties]
launch_timeout = { type = "string", default = "120s" }
launch_env     = { type = "string", pattern = "^[^']*$" }

[runtime.setup]
type = "exec"
bin  = "claude-runtime"
args = ["launch", "--timeout", { from = "inputs.launch_timeout" }]
```

```toml
[[review_agent.nodes]]
id   = "agent"
uses = "official.claude.runtime"

[review_agent.nodes.inputs]
launch_timeout = "180s"
path_prepend   = { from = "nodes.gh_app_guard.outputs.dir" }
```

This is a plugin and config decision. Prompt cannot reliably clean up a
half-launched process after unattended failure. Core cannot know what was
started before outputs existed. Config supplies deployment-specific duration
values, while plugin code owns the launch loop and the cleanup performed before
returning failure.

### 5. Split runtime rebuild from full rebuild

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Viable only after verify-before-skip covers the common repair path, but still leaves no explicit operator command for replacing a suspect runtime while preserving session work. |
| Keep one `--force-recreate` flag | core | Rejected. One flag cannot distinguish a safe runtime rebuild from discarding session-scoped work. |
| Replace it with a scoped recreate mode | core | Recommended. The CLI names the lifecycle scope the operator intends to discard. |

#### Recommendation

Plain `plect up` is the safe repair operation: it verifies produced nodes
before skipping and rebuilds only nodes whose liveness check failed and their
dependents.

The boolean `--force-recreate` is retired and replaced by an explicit scoped
mode:

```bash
plect up <session-or-resource> --recreate=run
plect up <session-or-resource> --recreate=all
```

`--recreate=run` cleans and forgets only run-scoped workflow nodes and runtime
observation state, then runs setup. It preserves session-scoped nodes,
conversation state, session inputs, dynamic task instances, done_when state,
and the event log.

`--recreate=all` cleans and forgets session-scoped and run-scoped workflow
nodes, dynamic task instances, health state, channel health state, tick
backoff, and runtime observation state while preserving the session identity,
resource mapping, parent relation, inputs, and event log. Operators use
`plect destroy` when they intend to remove the session rather than rebuild it.

This is a core decision. Prompt aliases cannot make the destructive boundary
visible to API callers, MCP callers, or scripts. Plugins do not own the
session task map. Config should not carry one-off operator intent.

There is no compatibility shim: scripts and documentation using
`--force-recreate` migrate once to `--recreate=run` or `--recreate=all`.

### 6. Population-produced sessions receive node-result events directly

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected. A population repair that keeps coarse run state `up` would remain silent to the channel waiting for repair progress. |
| Re-open the population `up` transition gate | core | Rejected. It would make `plect.workflow_population.up` lie about presence transitions. |
| Deliver node-result events through the member session's workflow channels | core, config | Recommended. Node attempts are ordinary session events and channels already declare the event types they relay. |

#### Recommendation

A population-produced session's node-result events are appended to that member
session's own event log. They are not population lifecycle events and do not
depend on `plect.workflow_population.up`, `down`, or any other desired-set
transition. A workflow channel that includes `plect.node.result` receives those
events whenever its input bindings can be resolved.

The population evaluator continues to emit `plect.workflow_population.up` only
for a genuine transition into run `up`. In-place liveness verification,
skipped nodes, liveness-triggered repair, and failed repair are expressed by
`plect.node.result`.

```toml
[[review_agent.event.channel]]
name    = "conversation"
uses    = "official.slack.slack"
include = [
  "plect.workflow_population.*",
  "plect.node.result",
  "plect.status_message",
]
```

This is a core and config decision. Core owns event production and the
population transition gate; config owns which session events are delivered to a
channel. A plugin cannot infer whether an `up` hook was a real population
transition, and a prompt cannot deliver events into a workflow channel.

## Consequences

`plect up` becomes a convergence operation rather than a produced-record
skip. A stale output no longer remains trusted merely because setup once
succeeded. The cost is that `up` can run liveness probes before returning; a
plugin author must keep those probes cheap and bounded.

Effect definitions with setup actions gain an explicit reuse obligation: either
declare `[health].alive` or declare `reuse = "record"`. This is a breaking
configuration-language change. The one-time migration is:

1. For every setup-bearing effect whose entity may safely be recreated, add a
   provider-owned `[health].alive` action that checks its reusable surface.
2. For effects whose entity Plecture deliberately does not check for
   persistence, add `reuse = "record"`.
3. Run the config-language conformance fixtures and plugin selftests for the
   changed plugins.

`plect ls --json` consumers retain the existing binary `run` enum. Health
reports now name failed or missing completed current-plan nodes. It never
interprets not-yet-attempted nodes during an active `up` as missing. A session
whose plan cannot be resolved retains its derived run state from task records
and reports the plan-resolution error as health evaluation failure for that
session; one broken workflow does not prevent other sessions from being listed.

Node-result events make setup progress and failure visible to session
channels, event subscribers, and population-produced sessions without changing
the meaning of population presence events. They also create one durable place
for bounded failure text, instead of relying on the foreground `plect up`
process as the only observer.

Failure-atomic setup remains a plugin obligation when side effects exist before
outputs. Core cannot safely clean up what it cannot identify, so plugin
selftests for launch effects should cover timeout cleanup as behavior tests
when those implementations land. This ADR does not add a standing CI check.

Retiring `--force-recreate` is a breaking CLI change. The one-time migration
is a script and documentation sweep from `--force-recreate` to the appropriate
scoped `--recreate` value. There is no compatibility alias.

This decision does not implement the described changes. Follow-up
implementation issues carry the code, tests, migrations, and schema updates
after owner ratification.

## Alternatives considered

### Add a separate validity mechanism

Rejected. The shipped catalog has 14 setup-bearing effects. Four already
declare `alive`: `pane`, `runtime`, `codex`, and `exec_runtime`. The migration
classifies the other ten without a third case: `gh_app_guard`,
`slack_subscribe`, `gh_guard`, and `worktree` gain `alive`; `thread_workspace`,
`codex_initial_prompt`, `claude_initial_prompt`, `slack_thread`,
`local_okf`, and `goal_bootstrap` declare `reuse = "record"`. No effect has a
reuse condition stricter than liveness. A `valid` action would therefore
duplicate an existing liveness probe or be unused, adding a lifecycle member
without a concrete consumer.

### Store liveness as a second durable truth

Rejected. Persisting a liveness result would create another stale latch. The
only durable truth remains the production record; liveness is re-evaluated at
the lifecycle decision point where reuse is about to happen.

### Treat every effect record as reusable

Rejected. A deleted Slack thread must not be recreated automatically because a
replacement splits the conversation, yet Plecture cannot confirm the old
thread persists. `reuse = "record"` makes that deliberate non-observation
visible. Without the marker, a missing probe would silently become an
unreviewed reuse policy.

### Add `degraded` to run state

Rejected. `run` already answers the binary capacity question: whether a
run-scoped surface is produced. A partial run remains `up`, and complete-plan
health makes it `unhealthy` with the missing or failed node named. This keeps
one meaning for `run` across child capacity, population presence, and cleanup.

### Emit only one run-level failure event

Rejected. A run-level event would help humans notice failure, but it would not
identify every node action, distinguish liveness-verified skips from repaired
nodes, or let a workflow channel render progress as the DAG advances. The node
is the lifecycle unit, so the event is node-scoped.

### Let workflows declare custom incomplete-health rules

Rejected. A failed or missing current-plan node is a core health fact.
Letting workflows redefine it would put two authorities behind the health
report and force dispatchers to re-learn each workflow's private meaning.

### Keep `--force-recreate` and add safer flags beside it

Rejected. The existing name does not say what state is discarded. Keeping it
would preserve the footgun and make scripts choose among overlapping flags.
A single scoped flag names the destructive boundary directly.
