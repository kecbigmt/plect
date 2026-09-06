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
