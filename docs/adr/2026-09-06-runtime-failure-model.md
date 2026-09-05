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
  a container replacement while produced records remain
  (kecbigmt/plecture#368).
- A runtime launch failure leaves later nodes unattempted, while the coarse run
  state can still read `up` because an earlier run-scoped node produced
  (kecbigmt/plecture#364, kecbigmt/plecture#370).
- A channel-side status can imply progress even when no runtime handoff occurs
  (kecbigmt/plecture#363).
- A workflow population `up` event is intentionally gated on a real not-up to
  up transition, so an in-place repair cannot rely on that event to reach a
  session's conversation channel (kecbigmt/plecture#394).

The decisions here keep output records as records of production. They add the
missing authorities that answer whether those records may be reused, whether a
run is fully usable, and what happened to each node attempt.

## Decision

### 1. Validity probes and verify-before-skip

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected. Operators can add recovery instructions, but a prompt cannot make `plect up` distinguish reusable outputs from stale outputs. |
| Require deployments to down then up before every resume | prompt | Rejected. It makes every caller duplicate lifecycle policy and still gives no per-node reason in state or events. |
| Use existing health probes as the skip authority | core | Rejected. Health answers whether a produced surface is alive and moving; reuse also applies to session-scoped effects and output validity that may not be part of session health. |
| Add effect validity probes and verify produced nodes before skipping | config, plugin, core | Recommended. Plugins own the executable knowledge of their surfaces, configuration declares the probe, and core owns the lifecycle decision to reuse or rebuild. |

#### Recommendation

An effect with a `setup` action declares a `valid` action unless it has no
external lifecycle surface to reuse. `valid` is an effect-level lifecycle
member beside `setup`, `cleanup`, `[health]`, and `[terminal]`. Its roots match
`cleanup`: `self.outputs.*`, `inputs.*`, `nodes.*`, `workflow.outputs.*`,
`session.*`, and `workspace.*`. Exit zero means the stored instance can be
reused. Non-zero exit, an unresolved required value, timeout, or invalid action
configuration means the instance is invalid.

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

[gh_app_guard.valid]
type = "shell"
script = 'test -x "$dir/gh"'

[gh_app_guard.valid.bind]
dir = { from = "self.outputs.dir" }

[gh_app_guard.outputs_schema]
type     = "object"
required = ["dir"]

[gh_app_guard.outputs_schema.properties]
dir = { type = "string" }
```

`plect up` verifies before it skips a produced workflow node. The walk is still
dependency ordered:

1. A produced node with no declared `valid` action is skipped only when the
   effect explicitly declares `validity = "record"`.
2. A produced node whose `valid` action succeeds is skipped.
3. A produced node whose `valid` action fails is marked failed with the
   validation error, then the node and its produced dependents are cleaned in
   reverse dependency order using their stored outputs.
4. Setup resumes from the first invalidated node in dependency order.

`validity = "record"` is a conscious declaration that the production record is
the whole durable truth. The loader rejects a setup-bearing effect that declares
neither `valid` nor `validity = "record"`, so missing validity is not silently
treated as reusable.

```toml
[write_instruction]
kind     = "effect"
scope    = "session"
validity = "record"

[write_instruction.setup]
type = "shell"
script = 'printf %s "$instruction"'
```

This is a config, plugin, and core decision:

- Prompt cannot enforce it because a human instruction cannot change
  lifecycle skip semantics.
- Config alone can describe the probe but cannot decide when to run it.
- Plugin code owns provider-specific checks such as testing a socket, process,
  generated wrapper, or subscription registration.
- Core owns the only safe place to compare the stored production record with
  the declared validity result and choose skip, cleanup, or setup.

Validation failure is not a health verdict. It is a lifecycle reuse verdict.
Health continues to report the state of produced run-scoped effects between
`up` calls.

### 2. Degraded run state

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected. Dispatchers would keep reconstructing partial failure by comparing task maps against workflow plans. |
| Fold partial failure into health | core | Rejected. A run can be structurally incomplete even when every declared health probe on the produced prefix passes. |
| Add `degraded` to the run state reported by `plect ls --json` | core | Recommended. Run state is the lifecycle completeness signal, not a probe signal. |

#### Recommendation

`domain.RunState` gains `degraded` as a third value. It is derived, not stored:

- `down`: no current-plan run-scoped node is produced.
- `up`: every current-plan run-scoped node is produced, and no current-plan
  run-scoped node is failed.
- `degraded`: at least one current-plan run-scoped node is produced and at
  least one current-plan run-scoped node is failed or missing.

A stale task entry for a node no longer in the workflow does not make the run
degraded; stale-node cleanup already handles that lifecycle. A failed
session-scoped node blocks create or repair, but it is not itself a run state.

`plect ls --json` exposes the derived value and a compact reason object. The
human table prints `degraded` in the RUN column and still prints health for the
produced run surface, because health and lifecycle completeness are distinct.

```json
[
  {
    "session_name": "kecbigmt/plecture-371+review_agent",
    "run": "degraded",
    "run_reason": {
      "failed": [
        {
          "node": "agent",
          "status": "failed",
          "error": "setup: launch timeout after 120s"
        }
      ],
      "missing": ["slack_subscribe"]
    },
    "health": "healthy",
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

No state-file migration is required for the recommendation because `degraded`
is computed from existing task records and the current plan. JSON consumers
must accept the new `run` enum value in the same release that documents it.

### 3. Node-result lifecycle events

#### Options

| Option | Layer | Assessment |
|---|---|---|
| Do not build this | prompt | Rejected. Foreground stdout is not observable by a waiting conversation or supervisor after the command exits. |
| Reuse `plect.workflow_population.failure` | core | Rejected. Node results are session lifecycle facts; they occur for manual sessions and child sessions as well as population members. |
| Emit one session event per node attempt | core | Recommended. The event log is the existing durable delivery surface for workflow channels and observers. |

#### Recommendation

Core emits `plect.node.result` to the affected session log whenever a workflow
node's setup, cleanup, or validity action completes or is skipped after a
successful validity check. The event is internal, sourced from `plect`, and
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
before skipping and rebuilds only invalid nodes and their dependents.

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
for a genuine transition into run `up`. In-place verification, skipped valid
nodes, invalid-node repair, and failed repair are expressed by
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
succeeded. The cost is that `up` can run validity probes before returning; a
plugin author must keep those probes cheap and bounded.

Effect definitions with setup actions gain an explicit validity obligation:
either declare `[valid]` or declare `validity = "record"`. This is a breaking
configuration-language change. The one-time migration is:

1. For every setup-bearing effect, add a provider-owned `[valid]` action that
   checks the reusable surface or outputs.
2. For effects whose production record is the whole truth, add
   `validity = "record"`.
3. Run the config-language conformance fixtures and plugin selftests for the
   changed plugins.

`plect ls --json` consumers must accept `"run": "degraded"` and may read
`run_reason` for the failing or missing nodes. No durable state rewrite is
required because the value is derived from the current workflow plan and
existing task records.

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

### Make health the only validity mechanism

Rejected. Health and validity answer different questions. Health asks whether
a produced run surface is alive and moving during observation. Validity asks
whether stored outputs may be reused during lifecycle convergence. A
session-scoped credential wrapper, a registration record, and a run-scoped
socket can all need validity checks even when they are not useful health
signals.

### Store validity as a second durable truth

Rejected. Persisting a validity result would create another stale latch. The
only durable truth remains the production record; validity is re-evaluated at
the lifecycle decision point where reuse is about to happen.

### Treat any failed node as `down`

Rejected. `down` means no run-scoped surface is produced. A partial run with a
live pane and a failed runtime is not down; it is degraded. Collapsing it to
`down` would hide the produced side effects that cleanup and inspection still
need to see.

### Emit only one run-level failure event

Rejected. A run-level event would help humans notice failure, but it would not
identify every node action, distinguish skipped-valid nodes from repaired
nodes, or let a workflow channel render progress as the DAG advances. The
node is the lifecycle unit, so the event is node-scoped.

### Let workflows declare custom degraded rules

Rejected. Degraded is a structural lifecycle fact: the current plan is not
fully produced. Letting workflows redefine it would put two authorities behind
one `run` value and force dispatchers to re-learn each workflow's private
meaning.

### Keep `--force-recreate` and add safer flags beside it

Rejected. The existing name does not say what state is discarded. Keeping it
would preserve the footgun and make scripts choose among overlapping flags.
A single scoped flag names the destructive boundary directly.
