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
dependency edge; `blocks` states a reverse edge. A workflow has no provider
node or special lifecycle.

`workdir`, when present, is exactly one projection from a node output:

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

[pull_review.nodes.inputs]
workspace_dir = { from = "nodes.checkout.outputs.workspace_dir" }
```

The workdir producer and all its transitive prerequisites are preparation
nodes. The graph determines that set before default-workdir dependencies are
added. Preparation actions execute in the invocation process directory. Every
other node runs in the declared directory and depends on its producer. If
`workdir` is omitted, no node receives a default directory. There are no
per-node or per-action cwd overrides.

Cleanup uses the directory chosen for its node setup. If that directory has
vanished, cleanup records a failure and does not run in any fallback directory.
This preserves cleanup actions' directory boundary. Down, repeated up, and
destroy use the same rule.

`[<id>.outputs]` is the workflow's explicit public projection record;
`outputs_schema` declares it. Each binding is evaluated from durable node
outputs after setup. `workflow.outputs.*` is persisted on the session and is
the only workflow-output root for display, instructions, node inputs, and
channel delivery. It replaces the former provider output path without creating
an `@workflow` pseudo-node.

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

## Populations and chains

A population belongs to a workflow and therefore derives its resource type
from the workflow. It cannot declare an independently authoritative resource.
Its query parameters satisfy the entry resource's query contract. It manages
the desired entry-resource sessions using its selected query means and declared
retention, down, and destroy policy. A population selects at most one initial
task compatible with its entry resource; it may omit it.

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

[[conversation.populations]]
name       = "open_conversations"
uses       = ["poll", "subscribe"]
poll_every = "5m"
auto_down  = true
auto_destroy = true

[conversation.populations.query]
status = "open"

[conversation.populations.session]
task = "conversation_triage"
```

The conversation population retains its directory and agent environment when
it adds an `issue_investigation` task bound to an issue resource. Its
capacity-driven down/up and guarded destruction remain population policy;
neither turns the issue into the session's entry resource.

## Validation rules

- `resource` resolves to one `resource` definition.
- A selected entry identifier resolves to that resource type.
- An omitted workflow is valid only with exactly one accepting workflow.
- `workdir` is a projection from a node output declared by this workflow.
- Node dependencies plus derived default-workdir edges have no cycle.
- A population uses its containing workflow's resource and its query contract.
- An initial task is caller-selected and compatible with its concrete binding.
- Workflow public outputs satisfy `outputs_schema` and bind only declared node
  outputs or allowed session inputs.
