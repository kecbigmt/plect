# Values

Wherever configuration accepts a dynamic value, it accepts these five forms:

```toml
literal      = "value"
required     = { from = "inputs.owner" }
with_default = { from = "inputs.model", default = "fable" }
omitted      = { from = "event.metadata.url", optional = true }
computed     = { expr = "event.body != '' ? event.body : event.summary" }
```

A TOML literal is already a value and takes no wrapper.

`from` is a Plecture projection, not CEL. It selects one statically
identifiable value from the evaluation context and preserves that value's
native type.

<!-- fixture: values/from.toml -->
```toml
[bootstrap]
kind  = "effect"
scope = "run"

[bootstrap.setup]
type = "exec"
bin  = "okf-goal"
args = [
  "task",
  "bootstrap",
  "--owner",
  { from = "inputs.owner" },
]

[bootstrap.health.alive]
type = "noop"

[bootstrap.inputs_schema]
type                 = "object"
required             = ["owner"]
additionalProperties = false

[bootstrap.inputs_schema.properties]
owner = { type = "string" }
```

A missing source is an error unless the value declares `default` or
`optional = true`. `default` supplies a value; `optional = true` propagates
absence, omitting the key rather than substituting a sentinel. The two are
mutually exclusive. No surface needs a fourth missing-value behavior.

`expr` holds a CEL expression and is reserved for actual computation:
conditionals, arithmetic, boolean operations, string construction. A pure
projection uses `from`, because Plecture reads that distinction directly for
dependency extraction, validation, and nested-output write-through. So

```toml
pid = { from = "inner.outputs.pid" }
```

is a projection, while

```toml
label = { expr = "'pid-' + string(inner.outputs.pid)" }
```

is a computation.

## Tagged values

The complete tagged-value vocabulary is:

```toml
{ from = "..." }
{ from = "...", default = ... }
{ from = "...", optional = true }
{ expr = "..." }
{ terminal = "send_text" }
{ bin = "codex-exec-enqueue" }
{ json = { from = "event" } }
```

`terminal` is a Plecture capability, resolved through whichever effect in the
plan declares the interactive endpoint. `bin` is a plugin-owned executable.
`json` is a boundary serializer, not a CEL function: its operand is a value
tree whose leaves are literals, projections, or computations.

```toml
body = { json = { from = "event" } }
```

A tagged value is used for non-computational Plecture semantics that the
containing field does not already communicate. Where a field's position
already determines its meaning, the field carries the fact directly:
`bin = "okf-goal"` on an exec action, not a generic value wrapper around the
same fact.

This vocabulary is not an expression language encoded as TOML. Arithmetic,
comparison, boolean logic, conditionals, and string computation belong in CEL.

A `{ json = ... }` operand is a TOML inline table, which TOML confines to a
single line. A payload that outgrows one line is expressed as a shell action's
binding instead, or as separate argv values the receiving executable assembles.

## Surfaces

A surface exposes only the context it is allowed to observe, rather than
receiving everything and relying on a later check to reject the rest.

| Surface | Roots |
|---|---|
| Resource `name` | `match.<capture>` |
| Resource `observe` | `resource.id`, `resource.inputs.<key>` |
| Resource `finalize` | `resource.id`, `session.name`, `resource.revision`, `judges`, `resource.inputs.<key>` |
| Resource `subscribe` | `session.name`, `resource.id`, `resource.inputs.<key>` |
| Resource `unsubscribe` | `session.name`, `resource.id`, `resource.inputs.<key>` |
| Resource query means | `inputs.<key>` |
| Workflow public `outputs` | `nodes.<id>.outputs.<key>`, `session.inputs.<key>` |
| Workflow population session `inputs` | `resource.id`, `item.<key>` |
| Workflow `display` | `workflow.outputs.<key>`, `session.inputs.<key>` |
| Workflow node `inputs` | `nodes.<id>.outputs.<key>`, `workflow.outputs.<key>`, `session.*`, `session.inputs.<key>` |
| Workflow event-channel `inputs` | same as workflow node inputs |
| Channel `args`, `path`, `body`, `bind` | `event.*`, `event.metadata.<key>`, `inputs.<key>`, and the terminal capability |
| Channel `timeout` | `inputs.<key>` only |
| Effect `setup` | `inputs.<key>`, `prev.<key>`, `nodes.<id>.outputs.<key>`, `workflow.outputs.<key>`, `session.*`, `session.inputs.<key>`, `resource.id` |
| Effect `cleanup` | `self.outputs.<key>`, `inputs.<key>`, `cleanup.inputs.<key>`, `force`, `nodes.<id>.outputs.<key>`, `workflow.outputs.<key>`, `session.*` |
| Effect `health` probes | `self.outputs.<key>`, `inputs.<key>`, `session.*` |
| Effect `terminal` verbs | `self.outputs.<key>`, `session.*` |
| Effect `inner.inputs`, `inner.env` | `inputs.<key>`, `locals.<key>`, `nodes.<id>.outputs.<key>`, `workflow.outputs.<key>`, `session.*` |
| Effect `outputs.bind` | `inner.outputs.<key>`, `locals.<key>`, `inputs.<key>` |
| Task `done_when`, chain `when` | `resource.state.<key>`, `self.state.<key>` |
| Task instruction body | `resource.id`, `resource.state.<key>`, `self.state.<key>`, `inputs.<key>`, `session.*`, `workflow.outputs.<key>` |
| Chain `inputs`, chain `resource` | `task.session`, `task.instance`, `task.workflow`, `task.done_when.pending_judge_ids`, `resource.state.<key>`, `self.state.<key>` |

A projection in a task document's instruction body is spelled `{{ <path> }}`
rather than as a tagged value, because prose has no room for a table; it is the
same projection, validated the same way, and stringified on the way out. See
[`tasks.md`](tasks.md).

A projection naming a root the surface does not observe is
`PLECTURE-CFG-FROM-ROOT`; one naming a field the resolved contract does not
declare is `PLECTURE-CFG-FROM-PATH`.

## Live roots and write-through

`resource.state.*` and `self.state.*` are live roots: a value reading one is current
as of each evaluation. Nothing about a value being "dynamic" needs its own
declaration form — re-evaluation rides on root liveness.

An evaluation is a pass that decides something: a completion predicate, a
chain condition. Such a pass observes the resource once, and every leaf of
that pass reads that one observation, so no two leaves of a single decision
disagree about what the resource says. Rendering a display is not an
evaluation: it reports the last observation, along with when that observation
was taken.

The live roots appear only in a task document, where a completion predicate and
a chain read them. An effect's outputs are production records, so its
`outputs.bind` observes no live root: what it reads is fixed when the layer is
instantiated, whether it comes from the nesting joint or from this layer's own
inputs.

```toml
[review]
kind              = "task"
description       = "Review a resource and record a verdict against its revision"
resource          = "issue_pr"
instructions      = [{ text = "Review {{ resource.id }} and record a verdict against its current revision." }]

[review.state_schema]
type = "object"

[review.state_schema.properties]
verdict_revision = { type = "string" }

[review.done_when]
all = [
  { check = "resource.state.resource_kind", in = ["pull", "issue"] },
  { expr = "self.state.verdict_revision == resource.state.revision" },
]
```

A direct projection of an inner output, `{ from = "inner.outputs.<key>" }`,
keeps write-through semantics for a mutable output: a later write to the inner
output reaches the outer contract. A computed value does not write through.

## Static topology

Fields that determine topology are never computed, so the shape of a
configuration is discoverable before anything is evaluated: `kind`, `resource`,
`uses`, `inner.uses`, a chain's `workflow`, and an exec action's
`command`. A tagged value on one of them is `PLECTURE-CFG-REF-DYNAMIC`.

## Validation rules

- `from` and `expr` together are rejected.
- `default` and `optional = true` together are rejected.
- A tagged value's keys come from the vocabulary above.
- A capability tag appears only on a surface that consumes capabilities: an
  action binding, or an argv element of an action that accepts one. A contract
  document declares types, not values, so no tagged value appears inside one.
- A channel `timeout` projects `inputs.*` only.
- An effect input obeys the effect's declared contract; a node binding may
  project an earlier node's output.
