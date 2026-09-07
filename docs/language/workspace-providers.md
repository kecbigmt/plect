# Workspace providers

A workspace provider owns everything a session needs to know about one kind of
resource, independent of any particular workflow: how a resource identifier
maps to a session name, how the workspace that resource needs is acquired and
released, and how a session binds to a watcher's subscription registry.

Workflows compose on top through `workspace_provider`.

## Resolution

`match` is a regular expression over the resource identifier. Its named
captures are the only root `name` observes, because `name` is resolved before a
session exists.

<!-- fixture: providers/match-name.toml -->
```toml
[worktree]
kind  = "workspace_provider"
match = '^https://github\.com/(?P<owner>[^/]+)/(?P<repo>[^/]+)/(?:issues|pull)/(?P<number>\d+)'
name  = { expr = "match.owner + '/' + match.repo + '-' + match.number" }

[worktree.setup]
type = "exec"
bin  = "github-worktree"
args = [
  "setup",
  "--resource",
  { from = "resource.id" },
  "--session",
  { from = "session.name" },
  "--workspace-dirs-root",
  { from = "config.workspace_dirs_root" },
  "--workspace-layout-root",
  { from = "inputs.workspace_layout_root", default = "" },
]

[worktree.cleanup]
type = "exec"
bin  = "github-worktree"
args = [
  "cleanup",
  "--workspace-dir",
  { from = "self.outputs.workspace_dir" },
  "--force",
  { from = "force" },
  "--delete-branch",
  { from = "cleanup.inputs.delete_branch", default = "" },
]

[worktree.health.alive]
type   = "shell"
script = 'test -d "$workspace_dir" && git -C "$workspace_dir" rev-parse --git-dir >/dev/null'

[worktree.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }

[worktree.subscribe]
type = "exec"
bin  = "github-watcher"
args = ["subscribe", "--session", { from = "session.name" }, "--resource", { from = "resource.id" }]

[worktree.unsubscribe]
type = "exec"
bin  = "github-watcher"
args = ["unsubscribe", "--session", { from = "session.name" }, "--resource", { from = "resource.id" }]

[worktree.inputs_schema]
type                 = "object"
additionalProperties = false

[worktree.inputs_schema.properties]
workspace_layout_root = { type = "string" }

[worktree.outputs_schema]
type     = "object"
required = ["workspace_dir", "branch"]

[worktree.outputs_schema.properties]
workspace_dir = { type = "string" }
branch        = { type = "string" }
title         = { type = "string", mutable = true }
```

## Hooks

| Hook | When |
|---|---|
| `setup` | Acquiring the workspace, on session create or up. |
| `cleanup` | Releasing it, on destroy. |
| `subscribe` | Binding a session to a resource at runtime. |
| `unsubscribe` | Dropping a session's binding to a resource. |

`setup` reads the resource, the session, the configured workspace root, and the
provider's own parameters. `cleanup` additionally reads the provider's recorded
outputs through `self.outputs.*`, the caller's cleanup inputs, and `force`.
`subscribe` and `unsubscribe` resolve the provider from the resource alone — no
workflow is in scope to have set a parameter — so each reads only session
context and the resource id: `unsubscribe` reads the session name;
`subscribe` additionally reads the session's own workspace branch
(`session.branch`, empty when the workspace provider that set up the session
produced none), so a provider can forward it to whatever delivery mechanism
it owns.

## Health

A workspace provider with `setup` declares `[health.alive]`: an executable
probe, as in the worked example above, or, when its produced record is
deliberately never re-observed, `type = "noop"`. `plect up` runs it against a
produced provider record before resolving the workspace cascade or compiling
the plan, and repairs a failed provider by running `cleanup` with `force =
true` at the force root, then `setup` again, mirroring the rebuilt outputs
into the session. A workspace provider has no scope and does not join the
periodic health cycle; unlike an effect's liveness, which composes into a
session's ongoing health verdict, a lost provider surface is repaired only by
the next `plect up` that observes it.

`[health]` admits `alive` only. `activity` is an unknown field: a provider
contributes no scope and casts no activity vote. `alive` observes the same
roots `cleanup` does, minus the ones a probe has no business reading: the
provider's own recorded outputs (`self.outputs.<key>`), its resolved inputs
(`inputs.<key>`), the session name, and the configured workspace-dirs root —
never `cleanup.inputs.*` or `force`, which belong to an explicit teardown
rather than a liveness check.

## Contracts

`inputs_schema` declares the provider's author-declared parameters, set by a
workflow's `workspace_provider_inputs`. Every one is data: wiring one can
change where and under what name a workspace lands, never what the hooks run.

`outputs_schema` declares the provider's output contract. A `mutable` property
may additionally be merged from a trusted side path; an output that a
best-effort fetch could not produce degrades to absent rather than to an empty
value.

## Validation rules

- `name` projects `match` captures only.
- A capture named in `name` exists in `match`.
- Provider parameters are data; no capability tag appears among them.
- `cleanup` reads `self.outputs.*` keys the provider's `outputs_schema` declares.
- A provider that declares `setup` declares `[health.alive]`.
- `[health]` admits `alive` only; `activity` is an unknown field.
- `health.alive` reads `self.outputs.*` keys the provider's `outputs_schema`
  declares, the same rule `cleanup` follows.
