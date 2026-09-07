# Verifying workspace-provider liveness at up

## Context

A workspace provider establishes the workspace before Plecture can resolve the
workspace cascade or compile a workflow plan. Its setup output is recorded as
the @workflow pseudo-node and its workspace_dir is mirrored into
session.workspace_dir_path. A produced pseudo-node is reused without an
existence check.

That makes a vanished workspace unlike a vanished workflow effect. Discovery
treats a missing workspace-directory layer as empty, so an ordinary plect up
can silently omit a workspace overlay and retain a dangling provider output.
A later action can fail when it uses that path, and a session with no such
action can still report a successful up. The workflow-effect liveness rule
already prevents that ambiguity for every setup-bearing effect: the declaration
names the fact that allows its production record to be reused.

The provider must name the same fact. Core cannot infer that a directory alone
is the provider's workspace: GitHub owns a Git worktree, Slack owns a thread
directory and conversation record, and the local knowledge-bundle provider
owns a scratch directory with a knowledge link. The setup hook is arbitrary,
so only the provider can say which of its output facts prove that its surface
still exists.

Repairing the provider before plan construction also resolves the apparent
cleanup-definition problem. When a workspace overlay vanished, core could not
select its old node cleanups. After provider repair recreates the workspace,
core reads that overlay again before it selects stale nodes or performs node
cleanup. A definition that remains unavailable is the existing fail-loud
cleanup case, not a reason to leave provider liveness unspecified.

## Decision

Every workspace provider declares [health].alive. It uses the same liveness
action vocabulary as an effect: shell, exec, or explicit noop. An executable
action exits zero only when the provider surface exists; noop means the
provider deliberately does not re-observe its production record. There is no
core-implicit path check and no new action type.

The declaration is mandatory because every workspace provider has setup. A
provider without [health].alive is a load error with the actionable form used
for setup-bearing effects: a workspace provider with setup declares
[health].alive, as an executable probe or an explicit noop. Health is part of
the workspace-provider kind, but it permits alive only; activity is an unknown
field. An alive action uses the ordinary action validation and binding rules.
It may read its stored self.outputs.* keys, which must be declared by the
provider output contract, along with the provider's allowed session, input, and
configured-root values.

| Provider field | Rule |
|---|---|
| [health] | Allowed only with alive. |
| [health].alive | Required when setup is declared; parses as shell, exec, or noop. |
| [health].activity | Rejected; a provider has no scope and never joins the health cycle. |
| self.outputs.<key> in alive | Allowed only for a key declared by outputs_schema, as in cleanup. |
| Missing alive | Load error rather than an implicit unchecked reuse. |

The shipped provider declarations use their own existence tests:

~~~toml
[worktree.health.alive]
type = "shell"
script = 'test -d "$workspace_dir" && git -C "$workspace_dir" rev-parse --git-dir >/dev/null'

[worktree.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }
~~~

~~~toml
[thread_workspace.health.alive]
type = "shell"
script = 'test -d "$workspace_dir"'

[thread_workspace.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }
~~~

~~~toml
[local_okf.health.alive]
type = "shell"
script = 'test -d "$workspace_dir"'

[local_okf.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }
~~~

plect up evaluates the provider action before plan construction. A fresh
session has no produced pseudo-node and runs provider setup as usual. For a
produced pseudo-node, a successful or no-op action reuses its output. A failed
action records the liveness failure, runs provider cleanup with force=true at
the force root, then runs provider setup. Core mirrors the rebuilt outputs into
the session and only then resolves the cascade and compiles the plan. Cleanup
or setup failure stops the up attempt with its persisted failure state.

Provider repair follows the node walk's invalidation rule. In
app/internal/task/task.go, RunSetup calls invalidateProducedNode from
app/internal/task/liveness.go when a produced effect cannot be reused. That
path does not compare old and new output maps: it cleans that node and every
transitive plan dependent before rebuilding them.
app/internal/service/lifecycle_up.go supplies the compiled plan to that walk;
the provider branch performs its check and repair before that compilation.
After a provider repair and plan construction, core applies that same rule to
each produced node whose input binds a provider output, directly through
workflow.outputs.* or through the provider-derived workspace.* values, and to
every transitive dependent of those nodes. It cleans those records before the
ordinary node liveness walk and setup. The rule applies even when the rebuilt
provider emits equal values; equality cannot prove that an old consumer still
refers to the rebuilt surface.

The provider is not a plan node. It has no scope, does not contribute activity,
does not run in periodic health evaluation, and does not escalate a failed
alive action. Its liveness failure is repaired only by the explicit plect up
operation that observed it.

An alive failure means the provider surface is gone or unusable. A provider
author keeps its action limited to that fact, rather than a transient or
readiness-like condition. The shipped tests above fail only in that condition.
A directory that remains a usable workspace passes, so provider cleanup does
not run and uncommitted work stays untouched. When the workspace is gone, no
local work remains for Plecture to protect. plect up --force-recreate remains
the operator's explicit whole-runtime rebuild for cases that require resetting
every runtime record.

## Consequences

The implementation adds the three shipped declarations and changes the
workspace-provider loader, kind surface, schema, and provider action validator
together. It adds provider-liveness execution before plan construction and
provider-dependent invalidation after plan construction. Tests cover missing
provider liveness, rejected provider activity, output-contract validation for
alive bindings, successful reuse, failed cleanup or setup, dependency cleanup,
and the three shipped actions. Existing effect-liveness tests continue to be
the specification for the common action and invalidation behavior.

This is a configuration migration, not a state migration. A provider owner
adds an executable probe or explicit noop; the three shipped providers add the
declarations above. An existing provider definition without one fails to load,
rather than retaining an unchecked compatibility path. Existing session
records need no rewrite: their stored outputs supply the first alive action,
and a failed check follows the repair lifecycle.

The meaning and reservation of workspace_dir remain outside this decision. The
[workspace-output design question](https://github.com/kecbigmt/plecture/issues/473)
may retain, revise, or remove that reservation. In every outcome, a workspace
provider still declares the liveness fact that authorizes reuse of its produced
record; only the output path through which an action reads that fact can change.

## Alternatives considered

### Leave recovery to --force-recreate

Rejected. It leaves workspace providers as the only setup-bearing kind whose
lost surface can be reused silently. The earlier cleanup-selection concern is
not unique to providers: after provider repair restores the cascade, ordinary
stale-node cleanup selects definitions; a definition that is still absent
fails loudly. An explicit whole-runtime reset remains useful, but it cannot be
the sole response to a liveness condition Plecture can now observe precisely.

### Implicitly test workspace_dir in core

Rejected. A path test is asymmetric with effect liveness and cannot establish
whether a provider's richer surface exists. It would also deepen a reservation
whose scope belongs to the [workspace-output design question](https://github.com/kecbigmt/plecture/issues/473),
rather than to provider lifecycle. A declared action lets each provider own
its real invariant without extending core vocabulary.
