# Invocation-selected project configuration context

## Context

Workspace-directory and ancestor discovery make a generated checkout decide
configuration after a workflow starts. Removing both without a replacement
would also discard legitimate repository policy: setup and teardown, owner
orchestrator outputs and instructions, and repository-specific review wiring.
The invoking directory can select that policy before any environment exists.

## Decision

An invocation resolves its configuration context before it resolves a resource
or acquires an environment. Starting at the invocation directory, `plect`
finds the nearest ancestor containing `.plect/project.toml`; that ancestor is
the project root. It reads only that root's `.plect/` definition tree. A
directory with no such marker selects no project tree. Ancestors above the
nearest root are never merged, and a generated checkout is never searched.

The selected context composes, in order, plugin definitions, machine-owned
global definitions, then the selected project definitions. Each later layer
replaces a whole same-kind definition; definition fragments do not merge. The
global layer is trusted by the local machine owner. A project tree is trusted
only when its root's canonical path is listed in `config.toml`'s
`trusted_project_roots`.

On an interactive invocation, an unlisted root displays its canonical path,
explains that its definitions can execute commands, and asks whether to trust
it. An affirmative answer adds that root to `trusted_project_roots` and
continues the same invocation; a negative answer stops it. A non-interactive
invocation stops with instructions to add the canonical root to that list.
Neither a chain nor a population can grant trust. Trust is per canonical root,
not per configuration digest; removing a root revokes its project definitions
from future desired operations. No trust failure falls through to a global
workflow.

The session records its selected project root and the layer revisions and
effective digest observed when it was created. The root, rather than the
caller's later cwd, remains the configuration-selection authority. The
definitions under that root remain editable: each desired operation loads the
latest valid configuration from the recorded root and compares its digest with
the recorded one. A difference is reported because changed declarations can
change reconciliation. It neither fails the operation nor destroys or rebuilds
anything by itself. Where a changed declaration requires rebuilding an existing
node, the diagnostic directs the caller to `--force-recreate`.

Each node also has an execution record, separate from the latest desired
workflow. That record retains its cleanup declaration, setup inputs and
outputs, execution directory, and resolved plugin version and reference,
including a partial or failed setup. Cleanup uses that record plus the current
operation's force and plugin-owned cleanup inputs. The session-state store owns
these records and protects their writes; it is the local trust boundary and
does not sign records. Resolved plugin executables and instruction sidecars
referenced by retained records stay available until the record is released.

If the recorded root is unavailable, operations requiring the latest desired
configuration fail with an actionable error and do not select another root.
Cleanup and release that can be performed from execution records remain
available. Chains inherit the triggering session's recorded root. Resident
populations resolve their desired configuration from the root recorded when the
resident started, not from the resident process's cwd; a population-created
session records that same root.

## Consequences

The workspace-directory cascade and arbitrary ancestor merging are retired.
Repository policy has one explicit root and one trust decision, while a Slack
orchestrator may retain a run-scoped multi-repository context without its
working directory becoming configuration input.

Migration backs up both global and project `.plect/` trees, places a
`project.toml` marker at each intended project root, records every root in
`trusted_project_roots`, consolidates ancestor fragments into the selected
tree, and removes workspace-derived overlays. The implementation must migrate
stored sessions to their selected roots and execution records before allowing
lifecycle operations; sessions whose old context cannot be reconstructed
require operator selection after backup. There is no fallback to an ancestor or
generated checkout.
The same procedure records the environment-effect and state migration in
[`resource-and-workspace-effects-migration.md`](../migrations/resource-and-workspace-effects-migration.md).

## Alternatives considered

### Global workflows only

Rejected. It makes repository-specific policy a manual copy and loses the
invocation's deliberate relationship to the project.

### Arbitrary ancestor merging

Rejected. More than one tree answers which policy controls one invocation and
expands the executable trust boundary with every parent directory.
