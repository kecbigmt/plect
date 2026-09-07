---
supersedes: 2026-08-17-workspace-provider-vocabulary
---

# Resource identity and workspace effects

## Context

`workspace_provider` combines resource resolution and subscription with
workspace lifecycle. It requires `setup`, accepts `match`, `name`,
`subscribe`, and `unsubscribe`, and reserves an immutable `workspace_dir`
output. [Its configuration](../../app/internal/config/workspace_provider.go)
and [loader](../../app/internal/config/workspace_provider.go) establish those
present rules.

Provider setup persists as the `@workflow` pseudo-node and requires a non-empty
`workspace_dir`; the lifecycle copies that output to the session. [The setup
hook](../../app/internal/task/workflowhook.go) and [session recreation](../../app/internal/service/lifecycle_up.go)
are the present authorities. `resource_observer` separately owns matching,
observation, finalization, state, and query. [Its configuration](../../app/internal/config/resource.go)
is the present authority.

## Decision

`workspace_provider` and `resource_observer` are replaced by one
`kind = "resource"`. A resource owns identity, observation, and delivery
binding: `match`, `name`, `observe`, `finalize`, `subscribe`, `unsubscribe`,
`inputs_schema`, `state_schema`, and `query`.

An ordinary `kind = "effect"` with `scope = "session"` acquires a session
workspace. Its setup, cleanup, liveness, and invalidation follow the ordinary
effect rules. The Slack thread provider becomes such an effect; it retains the
per-thread directory it creates today:

```toml
[thread_workspace]
kind  = "effect"
scope = "session"

[thread_workspace.setup]
type = "shell"
script = '''
rest=${session_name#slack/}
channel_id=${rest%-*}
ts_digits=${rest##*-}
thread_ts="${ts_digits%??????}.${ts_digits#??????????}"
workspace_dir="$workspace_dirs_root/slack/$channel_id/$thread_ts"
mkdir -p "$workspace_dir"
printf '{"workspace_dir":"%s","channel_id":"%s","thread_ts":"%s"}\n' \
  "$workspace_dir" "$channel_id" "$thread_ts"
'''

[thread_workspace.setup.bind]
session_name        = { from = "session.name" }
workspace_dirs_root = { from = "inputs.workspace_dirs_root" }

[thread_workspace.health.alive]
type   = "shell"
script = 'test -d "$workspace_dir"'

[thread_workspace.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }

[thread_workspace.inputs_schema]
type     = "object"
required = ["workspace_dirs_root"]

[thread_workspace.inputs_schema.properties]
workspace_dirs_root = { type = "string" }

[thread_workspace.outputs_schema]
type     = "object"
required = ["workspace_dir", "channel_id", "thread_ts"]

[thread_workspace.outputs_schema.properties]
workspace_dir = { type = "string" }
channel_id    = { type = "string" }
thread_ts     = { type = "string" }
```

Core drops the reserved `workspace_dir` output and the `@workflow` pseudo-node.
A workflow declares, once, which node output is the session's working
directory.

The deployment's `claude.toml` is not in this repository, so this diff uses the
shipped [review-thread workflow](../../plugins/slack/examples/review-thread-workflow.toml).
The worktree node is added; the existing node list is otherwise unchanged:

```diff
 [pr_review_with_slack]
 kind               = "workflow"
-workspace_provider = "official.github.worktree"
+resource           = "official.github.issue"
+workdir            = { from = "nodes.worktree.outputs.workspace_dir" }

+[[pr_review_with_slack.nodes]]
+id   = "worktree"
+uses = "official.github.worktree"

 [[pr_review_with_slack.nodes]]
 id   = "slack_thread"
 uses = "official.slack.slack_thread"

 [[pr_review_with_slack.nodes]]
 id   = "reviewer"
 uses = "official.codex.codex"
```

## Consequences

One migration rewrites the three shipped provider declarations:

- `plugins/github/config/workspaces/worktree.toml`
- `plugins/slack/config/workspaces/thread_workspace.toml`
- `plugins/okf/config/workspaces/local_okf.toml`

It also rewrites the deployment-owned orchestrator provider and each shipped
resource observer to `kind = "resource"`:

- [GitHub issue](../../plugins/github/config/resources/issue.toml)
- [GitHub pull request](../../plugins/github/config/resources/pull_request.toml)
- [OKF goal](../../plugins/okf/config/resources/okf_goal.toml)
- [Slack thread](../../plugins/slack/config/resources/thread.toml)

The migration inventory includes every `workspace.*` template reference,
`plect subscribe`, dispatch-time auto-subscribe, `plect up --force-recreate`,
the pseudo-node and storage importer, and the two repositories' `.plect/`
content. Core's direct reads of the branch output are removed separately.

## Alternatives considered

### Keep `workspace_provider` and make `workspace_dir` optional

Rejected. It preserves an exceptional lifecycle type after the directory is no
longer a core requirement.

### Keep resource observation separate from identity and delivery binding

Rejected. Two declarations matching one identifier would require agreement
rules for naming, binding ownership, and observation.
