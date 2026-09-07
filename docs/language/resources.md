# Resources

A resource declaration owns the facts and operations that describe one kind of
external thing. It recognizes an identifier, derives the session name for it,
observes its state, records a completed task when appropriate, and binds or
unbinds the session from the resource's delivery mechanism. A resource does
not acquire a directory and it does not run as a workflow node.

## Surface

| Field | Meaning |
|---|---|
| `match` | Required regular expression recognizing a resource identifier. |
| `name` | Required value over `match` captures that derives the session name. |
| `observe` | Required action producing the resource's current state. |
| `finalize` | Optional action recording completion and its judge evidence. |
| `subscribe`, `unsubscribe` | Optional actions binding and unbinding one session/resource pair. |
| `query` | Optional shared contract with `poll` and/or `subscribe` item sources. |
| `inputs_schema`, `state_schema` | JSON Schema contracts for resource inputs and observed state. |

`match` and `name` are one identity contract. A session created from a resource
uses the name value after the match succeeds. An explicitly selected workflow
must reference the resource that matches its supplied identifier. A resource
match that recognizes no identifier is valid; a supplied identifier that it
does not recognize is not. A workflow's optional `resource_inputs` literal
object satisfies the resource's `inputs_schema` and supplies `resource.inputs.*`
to its actions. Query means use their own `inputs.*` contract; the two input
namespaces do not overlap.

```toml
[github_issue]
kind  = "resource"
match = '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(?:issues|pull)/(?P<number>\d+)'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[github_issue.observe]
type = "exec"
bin  = "github-issue-pr"
args = ["observe", "--resource", { from = "resource.id" }]

[github_issue.subscribe]
type = "exec"
bin  = "github-watcher"
args = ["subscribe", "--session", { from = "session.name" }, "--resource", { from = "resource.id" }]

[github_issue.unsubscribe]
type = "exec"
bin  = "github-watcher"
args = ["unsubscribe", "--session", { from = "session.name" }, "--resource", { from = "resource.id" }]

[github_issue.state_schema]
type = "object"

[github_issue.state_schema.properties]
revision = { type = "string" }
title    = { type = "string" }
```

`observe` is the sole source of live `resource.state.*` facts. `finalize`
runs after completion has been reconfirmed and judge evidence gathered, so it
records rather than gates. A first-observation failure rejects instantiation;
a later failure is recorded degradation until a later observation succeeds.

`subscribe` and `unsubscribe` concern delivery, not lifecycle. They receive
the session and resource identity. An unavailable binding is retried through
the durable delivery queue; it does not turn a session directory into a core
concern.

## Query

`[<id>.query]` finds resources of this kind. Its required `inputs_schema`
describes a population's literal parameters, and its required `item_schema`
describes every item either means produces. At least one means is present:

- `poll` returns the complete matching set. Only a successful, fully validated
  poll proves absence.
- `subscribe` stays supervised and emits one JSON object per line. An item
  reports an appearance; silence, failure, and restart never prove absence.

The item schema has type `object`, requires only a string `resource` property,
and may declare optional identity or appearance context. Query items do not
duplicate properties from `state_schema`.

## Validation rules

- `match`, `name`, and `observe` are required.
- A name projection names only captures declared by `match`.
- `resource_inputs` satisfies the selected resource's `inputs_schema`.
- `finalize`, `subscribe`, and `unsubscribe` are optional.
- A query declares both shared schemas and at least one of `poll` and
  `subscribe`.
- A query item requires only its string `resource` property and declares no
  property also declared by `state_schema`.
- A task's `resource.state.<key>` projection names a `state_schema` property.
