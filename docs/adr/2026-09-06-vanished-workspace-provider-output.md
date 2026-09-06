# Recovering a vanished workspace-provider output

## Context

A workspace provider runs before Plecture can resolve the workspace cascade or
compile a workflow plan. Its successful setup is recorded as the `@workflow`
pseudo-node, and its `workspace_dir` output is mirrored into
`session.workspace_dir_path`. A later `plect up` reuses a produced pseudo-node.
The workflow-effect liveness walk cannot observe this record: a workspace
provider has neither a scope nor a `[health]` surface, and the plan it walks
depends on the provider output.

That ordering makes a lost workspace directory distinct from a lost workflow
effect. The stored output can name a worktree that disappeared with a
container, but core cannot reconstruct the old complete plan without first
recreating that directory. In particular, a workspace overlay may have added
nodes whose cleanup definitions no longer exist after the directory vanishes.
Core therefore cannot selectively invalidate every affected node, or safely
choose which external cleanup actions to omit, from the provider record alone.

The shipped providers demonstrate why a common “run setup again” rule is not a
provider contract. The GitHub provider's setup acquires a worktree and fetches
resource metadata. The Slack provider's setup creates a directory and records
a conversation. The local knowledge-bundle provider resolves another
session's workspace, creates a scratch directory, and replaces a symlink.
They are each safe to run only under the lifecycle their provider owns; the
configuration language does not require arbitrary providers to make setup
convergent on every `plect up`.

`plect up --force-recreate` is the explicit recovery lifecycle. It cleans
workflow task state in reverse setup order, invokes provider cleanup with the
force root, clears runtime state, runs provider setup, and compiles and sets up
the new plan. It retains the session identity, relationships, resource binding,
and event log. A cleanup failure leaves the session inspectable rather than
silently discarding its remaining state.

For the lost-directory case, the shipped provider cleanups converge. GitHub
prunes a missing worktree registration and reuses an orphaned branch during
the next setup. Slack's removal accepts an absent directory. The local
knowledge-bundle provider treats an absent scratch directory as released.

## Decision

Plecture does not add workspace-provider liveness or automatic repair.
Recovering a known-vanished workspace remains an explicit full-runtime
recreation:

```text
plect up <resource-id-or-session> --force-recreate
```

The command is intentionally not a health-cycle repair. A workspace provider
does not contribute to complete-plan health, does not escalate a failed
existence check, and has no scope. A provider record remains a production
record and has no periodic liveness authority.

Before running the command, an operator preserves any accessible work that is
not already lost. The force root reaches provider cleanup: for a GitHub
worktree that still exists, it can remove a dirty directory. A directory known
to have vanished has no local uncommitted work for Plecture to protect; any
such work was lost with the directory and cannot be recovered by a lifecycle
probe. The command's explicit flag is the acknowledgement that the entire
runtime, rather than a proved-safe subset of nodes, is being rebuilt.

No configuration surface changes. A provider keeps its existing setup and
cleanup shape; `[health]` remains outside the `workspace_provider` kind and is
a load error there.

```toml
[worktree]
kind = "workspace_provider"

[worktree.setup]
type = "exec"
bin = "github-worktree"

[worktree.cleanup]
type = "shell"
script = '"$worktree_bin" cleanup --workspace-dir "$workspace_dir" --force="$force"'

[worktree.cleanup.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }
force = { from = "force" }
```

The validation rules stay unchanged:

- `setup` is required for a workspace provider.
- `cleanup` may read declared `self.outputs` keys and the `force` root.
- `health` is not a workspace-provider field.
- The reserved `workspace_dir` output is non-empty after setup and is never
  mutable.

Existing sessions and providers need no migration. A produced provider record
continues to be reused by an ordinary `plect up`; an operator chooses
`--force-recreate` for the vanished-output case. Existing force-recreation
tests continue to specify state reset, ordered cleanup, setup failure, and
inspectable failure behavior. The GitHub workspace tests continue to specify
missing-worktree pruning and orphan-branch reuse. No behavior changes, so this
decision adds no test.

This is a core, plugin, and prompt boundary:

- Core owns the full teardown and rebuild operation, because it alone has the
  durable task records, dependency order, and runtime state to reset.
- A plugin owns the safety and convergence of its own setup and cleanup, but
  cannot determine whether every workflow effect using its output is safe to
  discard.
- A prompt can direct an operator to the existing explicit recovery command;
  it must not imply that a missing path proves which partial repair is safe.

## Consequences

The known recovery path is broader than replacing one directory. It deliberately
rebuilds task outputs, dynamic instances, environment state, runtime
observation state, and provider outputs. This avoids retaining a record whose
cleanup or inputs may refer to the lost workspace.

An ordinary `plect up` does not diagnose a vanished provider output before it
tries to load the workflow or plan. Its resulting failure is the signal to
inspect the workspace and choose the explicit recovery command. A provider
author who needs a narrower repair keeps that operation in its own plugin
until two concrete consumers establish a safe shared lifecycle contract.

The decision preserves the runtime-failure model's boundary: workflow effects
declare liveness because they participate in a plan and health cycle; workspace
providers establish the prerequisite for that plan. No state migration,
configuration migration, or new health report is introduced.

## Alternatives considered

### Optional provider `[health].alive`

Rejected. An action evaluated before plan construction could observe the
recorded provider outputs, but a non-zero result does not say how to repair
the workflow. It cannot resolve cleanup definitions that lived in the vanished
workspace overlay, and it cannot prove that retaining a session-scoped
subscription, terminal, or dynamic instance is safe after the workspace is
recreated. Treating the failure as a health result would also make a
scope-less provider participate in a complete-plan health model that has no
place to compose or escalate it.

The rejected shape would add a new configuration surface solely to detect a
condition whose safe response remains full recreation:

```toml
[worktree.health.alive]
type = "shell"
script = 'test -d "$workspace_dir"'

[worktree.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }
```

Making this shape valid would require provider-specific action validation,
stored-output roots, an execution point before cascade resolution, failure
persistence, and a rule for invalidating every node. It would still require
the explicit full teardown to protect external resources and uncommitted work.
It adds a second authority without removing the operator decision, so it does
not meet a present consumer need.

### Run provider setup on every `plect up`

Rejected. This changes a recorded production action into an unconditional
side effect. It requires every existing and user-authored provider setup to be
idempotent, despite no such contract today, and repeats metadata fetches and
conversation writes for the shipped providers. It also does not specify what
happens when setup emits a different `workspace_dir` while produced workflow
nodes still bind the old one. Re-running setup therefore cannot replace the
explicit teardown-and-rebuild lifecycle.
