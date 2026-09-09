# Workflows

A workflow assembles a session environment and declares exactly one entry
resource type. It does not declare the session's task. A task selected by the
caller may bind the entry resource or another concrete resource type.

## Session creation and identity

`plect up <resource>` resolves the concrete identifier to a resource type and
accepts `--workflow <id>` and optional `--tag <tag>`. The selected workflow's
`resource` must equal the resolved type. Without `--workflow`, exactly one
workflow accepting that type is required; zero or more than one is an error.

The resource's `name` creates its instance name. Without a tag, that value is
the session name. A tag appends one validated non-empty session-name segment;
the resulting name must either be new or name the same entry-resource and tag
record. Tags distinguish concurrent sessions for one resource without making a
task, plugin, or working-directory detail part of core identity.

The caller optionally selects an initial task and its concrete resource
binding. The task is compatible when its declared resource type matches that
binding. `plect up` may omit it, creating an environment a person may use.
Chains and populations use the same one-task selection surface, not a workflow
field. Adding or finishing a task never changes the entry resource, session
name, environment, or session lifetime.

## Nodes, work directory, and public outputs

Nodes are ordinary effects. A projection from `nodes.<id>.outputs.*` creates a
dependency edge; `blocks` states a reverse edge. A reference to
`workflow.outputs.*` expands to the producing node references in that public
output's binding, so a consumer depends on those producing nodes too. Public
outputs are evaluable as soon as their source node outputs exist; persistence
does not add a lifecycle node. The expanded edges participate in cycle detection
and in the explicit graph used to derive preparation nodes, before
default-workdir edges are added. A workflow has no provider node or special
lifecycle.

`workdir`, when present, is exactly one projection from a node output and must
resolve to an absolute local filesystem directory path. Environments without a
local directory omit it:

```toml
[pull_review]
kind     = "workflow"
resource = "pull_request"
workdir  = { from = "nodes.checkout.outputs.workspace_dir" }

[[pull_review.nodes]]
id   = "checkout"
uses = "checkout_effect"

[[pull_review.nodes]]
id   = "agent"
uses = "agent_runtime"

[[pull_review.nodes]]
id   = "orchestrator"
uses = "orchestrator_runtime"

[pull_review.nodes.inputs]
workspace_dir = { from = "nodes.checkout.outputs.workspace_dir" }
```

The workdir producer and all its transitive prerequisites are preparation
nodes. The graph determines that set before default-workdir dependencies are
added. Session creation records a preparation directory separately from its
configuration context: direct `plect up` records its caller's canonical working
directory; a chain inherits the triggering session's recorded preparation
directory; and a population inherits the directory recorded when its resident
started. Preparation setup, liveness, and cleanup actions use that recorded
directory, never a daemon's incidental cwd. Every other node's setup and
liveness actions run in the declared directory and depend on its producer. If
`workdir` is omitted, no node receives a default directory. There are no
per-node or per-action cwd overrides.

Cleanup uses the directory chosen for its node setup. If that directory has
vanished, cleanup is unavailable and does not run in any fallback directory.
This preserves cleanup actions' directory boundary. Down, repeated up, and
destroy use the same rule. A liveness probe whose launch needs that vanished
directory invalidates the node and attempts cleanup in the stored setup
directory; it never treats the missing directory as release evidence.

`[<id>.outputs]` is the workflow's explicit public projection record;
`outputs_schema` declares it. Each binding is evaluable from node outputs as
soon as its sources have been produced and is persisted on the session.
`workflow.outputs.*` is the only workflow-output root for display,
instructions, node inputs, and channel delivery. It replaces the former
provider output path without creating an `@workflow` pseudo-node.

```toml
[pull_review.outputs]
owner_endpoint = { from = "nodes.orchestrator.outputs.endpoint" }
instruction    = { from = "nodes.agent.outputs.instruction" }

[pull_review.outputs_schema]
type     = "object"
required = ["owner_endpoint", "instruction"]

[pull_review.outputs_schema.properties]
owner_endpoint = { type = "string" }
instruction    = { type = "string" }

[pull_review.display]
title  = { from = "workflow.outputs.instruction" }
status = "review"
```

## Event channels

`[[<id>.event.channel]]` selects a channel definition instead of an effect and
adds an `include` allowlist of event-type globs. Its `inputs` are values over
the same roots node inputs use, evaluated at delivery. `name` identifies the
binding within the workflow; two bindings may select the same channel under
different names and includes.

## Node lifecycle

`plect.node.result` is appended to a session's log whenever a node's setup,
cleanup, or liveness verification completes, or a produced node is skipped
after liveness passes. It applies equally to manual, child, and
population-produced sessions, independently of population up/down events.

| Metadata key | Meaning |
|---|---|
| `node` | The node id. |
| `effect` | The node's `uses` target. |
| `scope` | `session` or `run`. |
| `action` | `setup`, `cleanup`, or `alive`. |
| `result` | `produced`, `skipped`, `failed`, or `cleaned`. |
| `duration_ms` | How long the action took. |

`body` carries a bounded stderr or error tail only for `failed`; persisted
outputs remain the authority for produced and cleaned nodes.

## Desired workflow and execution records

The workflow loaded from the session's selected project root is the latest
desired workflow. It is reloaded for each desired operation and a population
reload. Before `up`, `down`, or `destroy` executes it is compared with the
session's shared lifecycle-configuration baseline. A change warns and then
executes the current trusted configuration; it never tears down or rebuilds a
node on its own.

Each setup attempt has a session-owned execution record, including partial and
failed attempts. It records the acquired resource identity, setup facts,
directory, nested-layer facts and environment, and the dependency edges and
allocation-lifetime information needed to release the existing plan. It does
not retain executable cleanup code, plugin binaries, a replayable cleanup
declaration, per-layer refusal digests, or plugin content pins. The local
session-state store protects writes to this evidence but is not a trust boundary
for executable code.

Retained execution records are a retained execution plan. Release follows its
recorded dependency order rather than an order derived from the latest desired
workflow: an old agent depending on an old checkout is cleaned up before that
checkout is released. This preserves the lifetime boundaries of allocations
whose declarations were removed or changed.

The current operation supplies `force` and plugin-owned cleanup inputs; they
do not replace setup-time facts. A record that still matches a desired node
remains in use. New nodes are set up from the latest desired
workflow. A changed node effect, resolved setup inputs, scope, or execution
directory requires reconstruction; the diagnostic directs the caller to
`--force-recreate`. A node removed from the desired workflow is not set up
again, but its record remains available for cleanup and release. Thus a
declaration revision takes effect immediately for future setup and population
policy, while an existing node's execution contract changes only by
reconstruction. Population reload follows the same rule: its current policy
controls future evaluation, while members and their provenance remain retained
as specified below.

## Cleanup and reconstruction

Before teardown, plect resolves the matching cleanup layer from the current
trusted configuration tree. A changed lifecycle-configuration baseline warns
before this execution but does not make cleanup unavailable. A missing
definition, valid project trust, setup fact, directory, credential, environment,
or reliable target identity is a cleanup precondition failure. The record stays
inspectable with its outstanding obligation and a non-secret reason; cleanup
never falls back to another directory or infers release.

Release follows execution-owned dependency edges, dependents before
prerequisites. A failed or unavailable dependent blocks release of its
prerequisites but not independent allocations. Successful cleanup marks only
that execution released. An externally released allocation requires the
explicit, audited acknowledgement operation proposed in [the cleanup
ADR](../adr/2026-09-08-minimum-cleanup-contract.md); its assertion is not a
successful cleanup and cannot release another generation.

Ordinary `up` can retry cleanup but cannot reconstruct an allocation or a
required prerequisite while its release obligation remains outstanding.
`--force-recreate` follows the same release-then-new-generation path and never
acknowledges or discards an obligation. `destroy --force` is the separate,
explicit record-discard operation: it warns and records that release was not
verified, then removes the session's remaining execution records. It is not
evidence that an external resource was released.

## Display

`[<id>.display]` declares values the CLI and Plecture Web UI render. They read
persisted public outputs only, never the network, so their freshness follows
the output update cadence.

## Clocks

`[<id>.tick]` declares when the tick reactor advances a session, in addition to
the judge builtin trigger. `on` lists event-type globs; `heartbeat` ticks after
that quiet duration; and `max_heartbeat` caps quiet-tick backoff. Omitting all
of them leaves manual ticks and the judge builtin as the only drivers.

`backoff_reset` names which conditions reset the quiet-tick backoff counter to
0 at a heartbeat sweep, holding the interval at `heartbeat`:

- `"inbound"` — an inbound event arrived since the last heartbeat sweep.
- `"fingerprint"` — the session's own composite done_when fingerprint changed.
- `"live_children"` — at least one session directly parented on this one is
  up. Read from the sessions table only; no probe, no child fingerprint.

Each declared condition is evaluated at both points the reactor consults the
backoff counter: before gating a heartbeat sweep (so a condition holding
*right now* makes the next tick due at `heartbeat`, not whatever interval the
counter had already grown to) and after a tick actually runs (so the counter
itself persists as reset). `"inbound"` and `"fingerprint"` hold when they
occurred since the last heartbeat sweep; `"live_children"` holds when it is
true at that instant.

The default, when `backoff_reset` is absent, is `["inbound", "fingerprint"]`.
Declaring the field replaces that default wholesale rather than adding to it,
so `backoff_reset = ["fingerprint"]` means inbound no longer resets. The list
must not be empty, and every name must be one of the three above — either
failure is a load-time error, since a tick that can never reset is a
misdeclaration. `[tick]` is workflow-level only: `config.toml` has no
defaults mechanism a workflow-level `[tick]` field falls back to, so there is
no global default for `backoff_reset`.

A heartbeat sweep's `kick` action body lists the session's up direct children
(name, run state, health, minutes since their own last tick) when any exist,
independent of `backoff_reset` — enough for the dispatcher receiving the kick
to decide which children it can safely bring down.

`[<id>.healthcheck]` declares `period`, `stall_threshold`, and
`renotify_every`. It controls sampling cadence, not what health means; effect
`[health]` declarations define that meaning. `tick` and `healthcheck` are
whole-table runtime tuning: a later workflow replacement replaces each table,
not individual keys.

## Concurrency

`max_up_children` optionally caps sessions parented on a session this workflow
produces that may hold run state `up` at once. `plect up` rejects a child that
would exceed the cap, naming the parent, cap, and current count; it does not
queue the request, so its caller retries after capacity frees.

A child counts while it holds run state `up` and stops counting when it goes
down or is destroyed. An admitted `plect up` in flight also counts until its
process is confirmed gone. An idempotent re-up of an already-up child is
exempt, because it is already counted. `--force-recreate` is not exempt: it
holds a new admission while rebuilding. A second up for the same child while
the first is running is rejected outright; once the first process is confirmed
gone, a retry reclaims the admission, and destroy clears it immediately.

An admission remains while its up process legitimately runs, not for a fixed
timeout. An omitted cap is unlimited.

When an admission — population, manual `plect up`, or a chain's dispatched
`plect up` — would exceed this cap, the reactor first looks, among this
workflow's real children of the same parent, for a session eligible under
`idle_down_after` (below) and currently clear; oldest activity then session
name breaks a tie. Finding one, it brings that session down through ordinary
cleanup and admits. Finding none, it rejects the admission as it does today.
`config.md`'s machine-wide `max_up_children` follows the identical rule
within its own, wider scope (every session with no real parent).

## Session idle-down and destroy policy

`[<id>.session]` declares idle-down and destroy policy for any session this
workflow produces — one dispatched by a chain, one a population admits, or
one a person creates directly with `plect up`.

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

| Field | Meaning |
|---|---|
| `idle_down_after` | Optional duration. Eligible sessions are downed automatically once idle this long. |
| `destroy.force` | Whether automatic destruction uses force; default false. |
| `destroy.inputs` | Optional plugin-owned cleanup input object. |
| `destroy.all` | Optional conjunction of `done_when`-style leaves (`tasks.md#completion`) over `resource.state.*`, gating automatic destruction. |

`idle_down_after`, when declared, makes a session eligible for an automatic
down: once its latest durable status is an explicit clear newer than its own
creation, its most recent accepted appearance, and every inbound event, and
it has stayed so for at least this duration, the reactor brings it down
through ordinary cleanup. Omitting the field means never — there is no
separate pin or keep flag, so a session that must stay up simply omits it.
A session with no real parent and no population provenance — one created
directly by `plect up`, never dispatched by a chain or admitted by a
population — is never eligible, whatever this declares: idle-down exists for
dispatched work, not for a session a person is using directly. A
population-admitted session is also parentless (it counts against the same
`max_up_children` key an operator's own session does), but its population
provenance marks it as dispatched work, so it stays eligible exactly like a
chain-dispatched real child. The same declaration is the sole authorization
for the capacity-pressure down described above and in `config.md`; there is
no separate boolean for it.

`destroy.all` reads only `resource.state.*` — there is no task instance
backing a session's own destroy policy, so no `self.state.*` to read — using
exactly the leaf kinds a `done_when` conjunction allows short of `judge`:
`check`/`in` and `expr`. Destruction waits until this predicate holds and,
independently, until every dynamic task instance on the session with its own
`done_when` is satisfied; a missing predicate, an observation failure, an
evaluation failure, or any pending leaf on either blocks it. A population's
absence tombstone (poll absence past `expire_after`) is a further, built-in
way to become eligible, flowing through this same guard rather than
bypassing it.

## Populations and chains

A population belongs to a workflow and therefore derives its resource type
from the workflow. It cannot declare an independently authoritative resource.
A population is deployment policy, declared only in user-owned global or
trusted selected-project configuration. Its identity is its stable
configuration-selection context, the containing workflow's resolved address,
and its unique `name`. The context is the selected canonical project root or
the distinct global-only context when no project root is selected. That
provenance is stored on every admitted session and is required for later
mutation or destruction.

| Field | Meaning |
|---|---|
| `name` | Required stable identifier, unique within the workflow. |
| `query` | Required literal parameters validated by the entry resource's query input schema. |
| `uses` | Required, non-empty query means, such as `poll` or `subscribe`. |
| `session.task` | Optional caller-selected initial task, compatible with the entry resource. |
| `session.inputs` | Optional values over literals, `resource.id`, and `item.*` properties. |
| `poll_every` | Required positive duration when `uses` selects `poll`; forbidden otherwise. |
| `expire_after` | Required positive quiescence duration without `poll`; forbidden with it. |

`uses` is the sole authority for query means. A population naming only `poll`
does not start subscribe even if the resource declares it; one naming only
`subscribe` does not poll. No default permits a later plugin-added means to
start in an existing deployment.

With `poll`, a complete validated snapshot is the sole membership and absence
authority. Subscribe appearances can admit or re-up a member but cannot undo a
poll absence tombstone; only a later positive poll opens a new generation.
Without `poll`, expiry measures successful session creation and resets only on
accepted repeated appearances or inbound session events; silence, failure, and
restart do not prove absence. Deselecting poll deliberately loses absence
detection and missed-event repair, so enumerable resources normally retain it.

Changing `uses` on config reload retains owned sessions and provenance, because
policy replacement is not resource evidence. The evaluator re-derives
membership using the new means alone. An invalid resident reload retains the
last valid evaluator.

A population-owned session's idle-down and destruction follow the containing
workflow's `[session]` policy (above) exactly like any other session this
workflow produces; a population declares no down/destroy fields of its own.
A poll absence past `expire_after` is this population's own additional
destroy trigger, flowing through that same `[session.destroy]` guard. An
appearance, inbound event, or positive poll requests up again for a session
the guard has not yet destroyed. Removing or invalidly changing provenance
never lets another population adopt existing sessions.

```toml
[standing_cases]
kind     = "workflow"
resource = "query_source"

[standing_cases.session]
idle_down_after = "30m"

[standing_cases.session.destroy]
force = false

[standing_cases.session.destroy.inputs]
delete_branch = false

[[standing_cases.populations]]
name       = "dispatch"
uses       = ["poll", "subscribe"]
poll_every = "5m"

[standing_cases.populations.query]
scope = "open"

[standing_cases.populations.session]
task = "population_task"

[standing_cases.populations.session.inputs]
context = { from = "item.context", optional = true }
```

Population decisions are durable events:

| Event | Meaning |
|---|---|
| `plect.workflow_population.up` | A member transitioned to up. Re-admitting an up member records nothing. |
| `plect.workflow_population.down` | Capacity policy selected or evaluated a down action. |
| `plect.workflow_population.destroy_deferred` | A task guard blocked destruction. |
| `plect.workflow_population.conflict` | Existing state has incompatible provenance. |
| `plect.workflow_population.failure` | A query or lifecycle operation failed. |

An eligible member's destruction itself is `lifecycle.destroyed`
(`events.md#session-lifecycle`), carrying `metadata.reason = "policy"` or
`"absence"` — there is no population-specific destroy event, and no dry-run
event: an eligible destruction always executes.

A task chain may start another session under a selected workflow once its
condition holds. It addresses the same resource by default or another concrete
resource it observed, such as a pull request created from issue work. The
target workflow must accept that resource. Judge acceptance remains associated
with the completion condition and revision/evidence contract that declared it.

## Representative configurations

The shared-environment pattern has specialized workflows for issue work and
pull review. The duplication is intentional: their entry resource types are
different even when their checkout, terminal, agent, and delivery nodes look
similar. Migrating one workflow from issues to pull requests creates a second
workflow, moves callers and populations to it, and retires the old one only
after its sessions drain.

```toml
[issue_work]
kind     = "workflow"
resource = "issue"
workdir  = { from = "nodes.checkout.outputs.workspace_dir" }

[[issue_work.nodes]]
id = "checkout"
uses = "checkout_effect"
[[issue_work.nodes]]
id = "agent"
uses = "agent_runtime"

[pull_review]
kind     = "workflow"
resource = "pull_request"
workdir  = { from = "nodes.checkout.outputs.workspace_dir" }

[[pull_review.nodes]]
id = "checkout"
uses = "checkout_effect"
[[pull_review.nodes]]
id = "reviewer"
uses = "agent_runtime"

[conversation]
kind     = "workflow"
resource = "conversation"
workdir  = { from = "nodes.thread_dir.outputs.workspace_dir" }

[[conversation.nodes]]
id = "thread_dir"
uses = "thread_directory"
[[conversation.nodes]]
id = "agent"
uses = "agent_runtime"

[conversation.session]
idle_down_after = "30m"

[conversation.session.destroy]
all = [{ check = "resource.state.status", in = ["closed"] }]

[[conversation.populations]]
name       = "open_conversations"
uses       = ["poll", "subscribe"]
poll_every = "5m"

[conversation.populations.query]
status = "open"

[conversation.populations.session]
task = "conversation_triage"
```

The conversation population retains its directory and agent environment when
it adds an `issue_investigation` task bound to an issue resource. Its
capacity-driven down/up and guarded destruction remain the workflow's
`[session]` policy; neither turns the issue into the session's entry
resource.

## Validation rules

- `resource` resolves to one `resource` definition.
- A selected entry identifier resolves to that resource type.
- An omitted workflow is valid only with exactly one accepting workflow.
- `workdir` is a projection from a node output declared by this workflow.
- A `workdir` resolves to an absolute local filesystem directory path.
- Node dependencies plus derived default-workdir edges have no cycle.
- A population uses its containing workflow's resource and its query contract.
- An initial task is caller-selected and compatible with its concrete binding.
- Population cleanup inputs satisfy the cleanup schemas of the effects they
  address.
- Workflow public outputs satisfy `outputs_schema` and bind only declared node
  outputs or allowed session inputs.
- `session.destroy.inputs` satisfies the cleanup schemas of the effects it
  addresses.
- Each `session.destroy.all` leaf is `check`/`in` or `expr`, never `judge`,
  and reads only `resource.state.*`.
