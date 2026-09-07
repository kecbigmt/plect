# Resources

A resource declaration owns one kind of external thing: recognition, instance
naming, observation, query, delivery binding, and finalization. A resource is
not a workflow node and does not acquire an environment.

## Identity and actions

`match` recognizes a concrete identifier; `name` derives that resource
instance's name from its captures. This name is distinct from a session name.
`observe` refreshes `resource.state.*`; it neither performs work nor implies
completion. `finalize` records a task's accepted completion and its evidence.
`subscribe` and `unsubscribe` bind or remove delivery for one session/resource
pair.

Each resource action takes a resource input object satisfying `inputs_schema`
when the resource declares one; no declaration means the empty object. The
object is supplied at the operation that needs it:

- `plect up` supplies it for the entry resource;
- adding a task supplies it for that task's concrete resource, including a
  resource of another type;
- a standalone observe, subscribe, unsubscribe, or finalize operation supplies
  it directly.

Resource action inputs are never inferred from workflow inputs or a previous
resource binding. Query inputs are separate: a population supplies the literal
object required by `[resource.query.inputs_schema]`, and query actions see it
as `inputs.*`, not `resource.inputs.*`.

```toml
[issue]
kind  = "resource"
match = '^https://forge\.example/(?P<project>[^/]+)/issues/(?P<number>\d+)$'
name  = { expr = "match.project + '-issue-' + match.number" }

[issue.observe]
type = "exec"
bin  = "forge"
args = ["issue", "observe", { from = "resource.id" }, "--token", { from = "resource.inputs.token" }]

[issue.subscribe]
type = "exec"
bin  = "forge"
args = ["issue", "subscribe", { from = "resource.id" }, { from = "session.name" }, "--token", { from = "resource.inputs.token" }]

[issue.inputs_schema]
type = "object"
required = ["token"]

[issue.inputs_schema.properties]
token = { type = "string" }

[issue.state_schema]
type = "object"

[issue.state_schema.properties]
revision = { type = "string" }
pr_url   = { type = "string" }
```

The first observation is required before instantiation; later observation
failure is recorded degradation. Finalization and delivery receive the
concrete resource, the concrete session where applicable, and the resource
action input object. A resource does not own a task's state or judge evidence.

## Query

`[<id>.query]` finds resource identifiers for a workflow population. It
declares an input schema and an item schema, and at least one of `poll` or
`subscribe`. A successful poll is the only absence authority; subscribe items
only report appearances. A query item has a string `resource` property and
optional appearance context. It does not duplicate observation state.

## Validation rules

- `match`, `name`, `observe`, and `state_schema` are required.
- A `name` projection names only captures declared by `match`.
- Every action operation receives an object satisfying `inputs_schema`.
- Query inputs satisfy the query input schema independently of action inputs.
- A query has its shared schemas and at least one means.
- A task's resource type matches the concrete resource to which its instance
  is bound.
