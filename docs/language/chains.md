# Chains

A chain is a deterministic rule for spawning task off a task document's
instances: once this instance reaches this state, run that workflow.

Chains live in the declaring task's own declaration because a chain's `when`
judge ids and `inputs` projections are references into that same document's
completion contract and observed keys — colocation is what lets those
references be checked at load time rather than at fire time. Evaluation is
scoped the same way: a chain fires only against instances of the task
document that declared it.

## Surface

| Field | Meaning |
|---|---|
| `id` | Identifies the chain within the declaring document. |
| `workflow` | The workflow to run. A static reference. |
| `placement` | Where the spawned session sits relative to this one. |
| `resource` | The concrete entry resource of the spawned session. |
| `when` | The facts that must hold for the chain to fire. |
| `inputs` | The session inputs handed to the spawned workflow. |

```toml
[pursue_goal]
kind              = "task"
description       = "Pursue one goal until an independent reviewer confirms it"
resource          = "goal"
instructions      = [{ text = "Pursue the goal at {{ resource.id }} until its checklist is satisfied." }]

[pursue_goal.done_when]
all = [
  { check = "resource.state.checklist_status", in = ["SUCCESS"] },
  { judge = "goal is achieved according to the goal file and event evidence", id = "goal-met", relation = ["sibling"] },
]

[[pursue_goal.chains]]
id        = "goal_review"
workflow  = "goal_reviewer"
placement = "sibling"

[pursue_goal.chains.when]
all = [
  { check = "resource.state.checklist_status", in = ["SUCCESS"] },
  { judge_pending = "goal-met" },
]

[pursue_goal.chains.inputs]
task         = "goal_review"
work_session = { from = "task.session" }
instance     = { from = "task.instance" }
judge_ids    = { from = "task.done_when.pending_judge_ids" }
```

## Triggers

`when.all` is a conjunction of facts, reading the same roots a completion leaf
does. A check fact compares one key and an expression fact states a computed
predicate, both exactly as in `done_when`. `judge_pending` holds while a named
judge leaf is still awaiting a verdict — which is how a chain spawns the
reviewer that will supply it. `judge_action` with `is` matches a recorded
verdict.

## Inputs

Chain inputs project the facts of the instance that fired — which session and
instance it was, which workflow, its pending judge ids — and the live roots it
reads, `resource.state.*` and `self.state.*`.

They read public facts only. An effect layer's private locals do not cross into a
spawned session.

## The spawned session's resource

`resource` names what the spawned session is about. It is a value over the
same roots `inputs` reads, because it is one of the facts the firing instance
projects — what separates it is that the spawned session is *bound* to it
rather than handed it. The workflow's resource then resolves it exactly as it
resolves a resource dispatched directly.

Omitting it binds the spawned session to the declaring session's own resource.
That default is what a chain spawning more work on the same subject wants; a
chain whose spawned session is about something else says so.

```toml
[work]
kind              = "task"
description       = "Implement a fix and hand the pull request to a reviewer"
resource          = "issue_pr"
instructions      = [{ text = "Resolve the issue at {{ resource.id }} and open a pull request." }]

[work.done_when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge = "acceptance criteria are satisfied", id = "ac-met" },
]

[[work.chains]]
id        = "review"
workflow  = "goal_reviewer"
placement = "sibling"
resource  = { from = "resource.state.pr_url" }

[work.chains.when]
all = [
  { check = "resource.state.checks_status", in = ["SUCCESS"] },
  { judge_pending = "ac-met" },
]

[work.chains.inputs]
task         = "goal_review"
work_session = { from = "task.session" }
judge_ids    = { from = "task.done_when.pending_judge_ids" }
```

A reviewer's entry resource is the pull request, and the work session's entry
resource is the issue that asked for it. The chain's target workflow must
declare the pull-request resource type. Naming the resource is what lets one
chain say both — the fact the reviewer is spawned against is a fact the work
instance observed.

Resolution is fail-closed at fire time. A `resource` that reads a key nothing
has reported yet, or that resolves to an empty string, blocks the fire rather
than spawning a session bound to nothing: the pull request does not exist yet,
so neither does the review. The chain fires on the tick after the fact
arrives.

## Static workflow references

`workflow` is a static reference. Templated or otherwise computed workflow
selection is not part of the language: a chain's target is topology, and finite
dynamic selection would be its own language decision with its own concrete
consumer.

## Validation rules

- `workflow` resolves to a definition of kind `workflow`.
- `workflow` is never a computed value.
- The target workflow accepts the resolved chain resource.
- `resource` projects the same roots `inputs` does.
- A `resource` that resolves to nothing, or to an empty string, blocks the
  fire.
- A `when` judge id names a judge leaf this document's `done_when` declares.
- A check fact names a key the declared resource publishes, or one this
  document's `state_schema` declares.
- Chain inputs project public work facts, not locals.
- Chain inputs satisfy the target workflow's `inputs_schema`.
