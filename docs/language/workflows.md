# Workflows

A workflow is a named bundle of nodes plus the event channels, display values,
and clocks for the sessions it produces. It selects a workspace provider; the
provider owns the resource-kind knowledge, and the workflow owns the effect shape
on top of it. A workflow without one cannot acquire a workspace, so it cannot
back a session.

## Nodes

Each node selects an effect through `uses` and binds that effect's inputs. A node's
`id` defaults to the referenced definition's id.

The setup and cleanup graph is derived from the node bindings: a projection
rooted at `nodes.<id>.outputs` is a dependency edge. There is no `depends_on` —
wiring data is what declares order.

`blocks` declares the reverse edge, making each listed node a dependent of this
one. It exists for a cascade overlay that must insert itself ahead of base
nodes it cannot modify.

<!-- fixture: workflows/nodes.toml -->
```toml
[goal_reviewer]
kind               = "workflow"
name               = "goal-review agent (local-okf)"
description        = "Dispatch an agent session against a local-okf goal resource, then deliver the goal_review task's instructions."
workspace_provider = "okf_bundle"

[goal_reviewer.display]
title  = { from = "workflow.outputs.concept_id" }
status = "goal_review"

[goal_reviewer.inputs_schema]
type                 = "object"
required             = ["task"]
additionalProperties = false

# A chain's inputs are session inputs, so this schema is the contract they are
# checked against: the work facts the chain hands its reviewer are declared
# here, not passed around it.
[goal_reviewer.inputs_schema.properties]
work_session = { type = "string" }
instance     = { type = "string" }
judge_ids    = { type = "array", items = { type = "string" } }

[goal_reviewer.inputs_schema.properties.task]
type        = "string"
description = "Initial task to instantiate for this session."
enum        = ["goal_review", "none"]

[[goal_reviewer.nodes]]
uses = "pane"

[[goal_reviewer.nodes]]
id   = "worker"
uses = "official.codex.exec_runtime"

[[goal_reviewer.nodes]]
uses = "initial_task"

[goal_reviewer.nodes.inputs]
task      = { from = "session.inputs.task", default = "" }
queue_dir = { from = "nodes.worker.outputs.queue_dir" }

[[goal_reviewer.event.channel]]
name    = "runtime"
uses    = "official.codex.exec_delivery"
include = ["plect.instruction", "user.emit", "plect.terminal.*"]

[goal_reviewer.event.channel.inputs]
queue_dir = { from = "nodes.worker.outputs.queue_dir" }

[goal_reviewer.tick]
on        = ["resource.*"]
heartbeat = "15m"
```

## Event channels

`[[<id>.event.channel]]` selects a channel definition instead of an effect and
adds an `include` allowlist of event-type globs. Its `inputs` are values over
the same roots node inputs use, evaluated at delivery.

`name` identifies the channel binding within the workflow; two bindings may
select the same channel definition under different names and includes.

## Display

`[<id>.display]` declares the values the CLI's listing and show commands, and
the web UI, render.
They read persisted outputs only — no network — so their freshness follows
whatever cadence updates those outputs.

## Clocks

`[<id>.tick]` declares when the tick reactor advances a session, on top of the
judge builtin trigger. `on` is a list of event-type globs whose match ticks the
session; `heartbeat` ticks when that long has passed since the last tick;
`max_heartbeat` caps the quiet-tick backoff interval. Both fields are optional
and independent: neither declared means a manual tick and the judge builtin
are the only drivers.

`[<id>.healthcheck]` declares the health sampling cycle: `period`,
`stall_threshold`, and `renotify_every`. It names the cycle, not what health
means; what each probe observes is an effect-level `[health]` declaration.

Unlike the rest of a workflow's fields, `[tick]` and `[healthcheck]` are
whole-table runtime tuning: a deeper cascade layer replaces a shallower layer's
table wholesale rather than merging into it.

## Concurrency

`max_up_children` optionally caps how many sessions parented on a session
this workflow produces may hold run state "up" at once — for example, an
orchestrator workflow declaring `max_up_children = 7` limits itself to 7
concurrently-up child sessions. `plect up` rejects a child that would push
its parent past the cap, naming the parent, the cap, and the current count;
it does not queue the request, so the caller (typically an orchestrator
tick) retries on a later cycle once a child frees up.

Counting rule: a child counts toward the cap while it holds run state
"up" — any run-scoped task produced — and stops counting the moment it goes
down or is destroyed. A `plect up` in flight against the same parent, admitted
but not yet up itself, also counts, closing the window between two
concurrent `plect up` processes deciding at the same instant; this is why a
rejection's reported count can briefly exceed what `plect ls` shows. A
`plect up` that only brings an already-up child back up again (an idempotent
re-up) is exempt from the cap, since that child is already counted.
`--force-recreate` is not this case even on an already-up child: it tears
run state down before rebuilding it, so it holds an admission of its own
for that whole window rather than being exempt.

An in-flight admission survives however long its `plect up` process
legitimately keeps running — it counts until that process is confirmed
gone, not until some fixed time passes. A second `plect up` for that same
child while the first is still running is itself rejected outright, not
queued behind it; once the first process is confirmed gone (killed rather
than finished), a retry on that child reclaims the admission, and `plect
destroy` on it clears the admission immediately either way.

Unset means no cap.

## Populations

`[[<id>.populations]]` declares a desired set of parentless sessions under a
user-owned workflow. A population is deployment policy, so plugins and cloned
workspace overlays cannot declare it. Its identity is the containing
workflow's resolved address plus its unique `name`; that provenance is stored
on every admitted session and is required for later mutation or destruction.

| Field | Meaning |
|---|---|
| `name` | Required stable identifier, unique within the workflow. |
| `resource_observer` | Required static reference to the observer that recognizes, queries, and observes population resources. |
| `query` | Required literal parameter object validated by the observer query's `inputs_schema`. |
| `uses` | Required, non-empty selection of query means this entry runs, e.g. `["poll"]`. Each keyword must be one the resolved observer's query declares. |
| `session.task` | Optional static initial task, installed with the name `initial`; it uses the same observer. |
| `session.inputs` | Optional values over literals, `resource.id`, and properties declared by the query `item_schema` under `item.*`. |
| `session.destroy.force` | Whether an enabled automatic destruction uses force; defaults to false. |
| `poll_every` | Required positive duration when `uses` selects `poll`; forbidden otherwise. |
| `expire_after` | Required positive external-input quiescence duration when `uses` does not select `poll`; forbidden when it does. |
| `auto_down` | Permits capacity-pressure down selection; defaults to false. |
| `auto_destroy` | Permits guarded automatic destruction; defaults to false, producing a dry-run verdict instead. |

`uses` is the sole authority for which means run: an entry naming only
`poll` never starts the observer's subscribe action even when the observer
declares one, and an entry naming only `subscribe` never runs poll, even
against an observer whose query declares both. There is no default
selection, so a means a plugin observer adds later cannot start running in an
existing deployment until an entry names it explicitly.

Selecting `poll` makes it the sole membership and absence authority for that
entry: a complete snapshot is validated before membership changes, and
subscribe items (when also selected) admit or re-up a member but cannot undo
an absence tombstone; only a later positive poll opens a new generation.
Deselecting `poll` follows the subscribe-only rules instead: expiry is
measured from successful session creation and reset by accepted repeated
appearances or inbound session events, and a resource is never presumed
absent. Internal, outbound, and status events do not reset that clock.

Deselecting `poll` on an observer whose query declares it is a deliberate
trade: it drops absence detection and missed-event repair (a subscribe
delivery gap is never independently corrected) in exchange for not running a
poll means that is too expensive, rate-limited, or unavailable for this
deployment. An enumerable resource should normally keep `poll` selected;
deselecting `subscribe` instead is the more common case, e.g. for a
deployment with no reachable webhook endpoint to run a fail-closed subscribe
action against, which would otherwise become a permanent restart loop under
the resident supervisor.

Changing an entry's `uses` on a config reload keeps its owned sessions and
their provenance — removal or replacement of policy is never evidence about
a resource — but the evaluator re-derives membership from the new
selection's authority alone. A resource tombstoned under a since-deselected
`poll` is admitted again by a later subscribe appearance instead of staying
locked out waiting for a poll snapshot that will never come.

Destruction waits until every produced dynamic task with a `done_when`
predicate is satisfied. A missing predicate, observation error, evaluation
error, or pending leaf blocks destruction. `auto_destroy = false` records the
same eligible decision without applying it.

At virtual-root capacity, only an up, population-owned session from an entry
with `auto_down = true` is eligible. Its latest durable status event must be an
explicit clear newer than its creation, last accepted appearance, and last
inbound event. Eligible sessions are selected by oldest activity, then session
name, and are brought down through ordinary run-scoped cleanup. An appearance,
inbound event, or positive poll generation requests ordinary up again.

`populations` is a whole-array cascade field: a deeper user-owned workflow
declaration replaces the shallower array rather than merging entries. Removing
or invalidly changing provenance never authorizes a differently named entry to
adopt existing sessions. A resident config reload that fails validation keeps
the last valid evaluator running.

<!-- fixture: workflows/populations.toml -->
```toml
[standing_cases]
kind               = "workflow"
workspace_provider = "query_provider"

[standing_cases.inputs_schema]
type                 = "object"
required             = ["resource"]
additionalProperties = false

[standing_cases.inputs_schema.properties]
resource = { type = "string" }
context  = { type = "string" }

[[standing_cases.populations]]
name              = "dispatch"
resource_observer = "query_source"
uses              = ["poll", "subscribe"]
poll_every        = "5m"
auto_down         = true
auto_destroy      = false

[standing_cases.populations.query]
scope = "open"

[standing_cases.populations.session]
task = "population_task"

[standing_cases.populations.session.inputs]
resource = { from = "resource.id" }
context  = { from = "item.context", optional = true }

[standing_cases.populations.session.destroy]
force = false
```

Population lifecycle decisions are durable events:

| Event | Meaning |
|---|---|
| `plect.workflow_population.up` | A member's session transitioned to up. Re-admitting an already-up member records nothing. |
| `plect.workflow_population.down` | Capacity policy selected or evaluated a down action. |
| `plect.workflow_population.destroy` | An eligible member was destroyed. |
| `plect.workflow_population.destroy_deferred` | The task guard blocked destruction. |
| `plect.workflow_population.destroy_dry_run` | Destruction was eligible but automatic execution was disabled. |
| `plect.workflow_population.conflict` | Existing state has incompatible provenance. |
| `plect.workflow_population.failure` | A query or lifecycle operation failed. |

## Provider parameters

`[<id>.workspace_provider_inputs]` sets the provider's author-declared
parameters. The values are literal data: the provider's hooks run before any
workspace or node output exists, so there is nothing for a projection to read.

## Validation rules

- `workspace_provider` and every node's `uses` resolve to a definition of the
  expected kind.
- Two nodes may not share an id, including after `id` defaulting.
- Dependencies derived from `nodes.<id>.outputs` projections form no cycle.
- A node input projecting `nodes.<id>.outputs.<key>` names a node in this
  workflow and an output that node's effect declares.
- `workspace_provider_inputs` values are literals.
- A cascade layer may add fields but not redeclare one a shallower layer set,
  except for the whole-table clocks.
