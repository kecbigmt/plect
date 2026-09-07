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
`trusted_project_roots`; an unlisted project root is reported and contributes
no definitions. This makes the project selection visible and rejects a
directory traversal as an implicit source of executable configuration.

The resolved context records its project root, layer revisions, and effective
configuration digest in the session. Later `up`, `down`, `destroy`, task
addition, observation, and delivery use that record, not the caller's current
directory. A config reload can change policy only for subsequently created
sessions; an existing session keeps its recorded digest until it is destroyed.
If the recorded tree cannot be read or no longer matches its digest, an
operation fails rather than selecting a different configuration.

Chains inherit the triggering session's recorded context unless their target
workflow is absent there; then firing fails. Resident populations resolve their
context from the project root recorded when the resident started, not from the
resident process's cwd. A population-created session records that same context.

## Consequences

The workspace-directory cascade and arbitrary ancestor merging are retired.
Repository policy has one explicit root and one trust decision, while a Slack
orchestrator may retain a run-scoped multi-repository context without its
working directory becoming configuration input.

Migration backs up both global and project `.plect/` trees, places a
`project.toml` marker at each intended project root, records every root in
`trusted_project_roots`, consolidates ancestor fragments into the selected
tree, and removes workspace-derived overlays. The implementation must migrate
stored sessions to a recorded context before allowing lifecycle operations;
sessions whose old context cannot be reconstructed require operator selection
after backup. There is no fallback to an ancestor or generated checkout.
The same procedure records the environment-effect and state migration in
[`resource-and-workspace-effects-migration.md`](../migrations/resource-and-workspace-effects-migration.md).

## Alternatives considered

### Global workflows only

Rejected. It makes repository-specific policy a manual copy and loses the
invocation's deliberate relationship to the project.

### Arbitrary ancestor merging

Rejected. More than one tree answers which policy controls one invocation and
expands the executable trust boundary with every parent directory.
