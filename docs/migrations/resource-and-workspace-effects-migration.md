# Resource and workspace effects migration

This procedure moves a configuration tree to the resource-entry and ordinary
environment-effect dialect. It is a breaking migration: `workspace_provider`
and `resource_observer` have no aliases, provider output records have no
compatibility reader, and a session is never reconfigured from its current
directory.

Read the whole procedure before making changes. Perform it after the release
that implements the dialect; this design PR does not implement it.

## Backup

Stop resident processes before copying their configuration and durable state.
The backup must include the global tree, every selected project tree, and the
state directory because existing sessions need a recorded context and migrated
node/output records.

```bash
CONFIG_ROOT="${PLECT_CONFIG_HOME:-$HOME/.config/plect}"
DATA_ROOT="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$CONFIG_ROOT" "$CONFIG_ROOT.resource-effects-backup.$STAMP"
cp -a "$DATA_ROOT" "$DATA_ROOT.resource-effects-backup.$STAMP"
```

For each project root, copy its `.plect/` tree to a sibling backup before
placing a project marker or consolidating configuration.

`PLECT_CONFIG_HOME` is the supported override for the global configuration
tree. Durable state is under `XDG_DATA_HOME` as shown above; this procedure
does not name a state-directory override.

## Convert declarations and workflows

1. Rewrite every `kind = "resource_observer"` as `kind = "resource"`, adding
   `name`, delivery actions, and the resource action input schema where the
   former provider owned them.
2. Rewrite every `workspace_provider` as a resource declaration plus one or
   more ordinary `scope = "session"` effects. Move worktree, Slack-thread,
   and local environment setup, cleanup, liveness, and outputs to those
   effects. Preserve cleanup inputs such as `force` and plugin-owned options.
3. Replace a workflow provider field with its entry `resource`, add nodes for
   its effects, and add one `workdir = { from = "nodes.<id>.outputs.<key>" }`
   where the workflow needs a directory. Do not create per-node cwd settings.
4. Replace `@workflow` output consumers with explicit workflow public
   outputs and `outputs_schema`; rewrite display, instructions, channel, and
   downstream bindings to `workflow.outputs.*`.
5. Move a workflow's initial task selection to each invocation, chain, or
   population. Ensure each task's concrete resource matches the task document
   and ensure a population derives its resource from its workflow.

## Establish project contexts

For every intended project configuration, choose one project root, consolidate
only that root's intended `.plect/` definitions, and add this marker:

```toml
schema_version = 3
```

It contains no definitions or other fields and declares only the selected
project layer's dialect. Add the root's canonical path to
`trusted_project_roots` in the machine configuration. Do not preserve ancestor
fragments or copy configuration from a generated checkout.

Before upgrading, release every unreleased allocation with the pre-upgrade
binary where that is possible. Do not copy cleanup code or plugin files into
durable state to make the later release run.

Migrate existing sessions to their recorded project root, initial layer
revisions, and node execution records before running lifecycle operations. A
legacy session starts with no lifecycle-configuration baseline. Its first `up`,
`down`, or `destroy` execution records the current trusted lifecycle
configuration without a change warning; no historical digest or plugin hash is
required for cleanup.

Preserve every concrete execution fact already recorded. Do not manufacture a
missing declaration identity, cwd, setup input, output, nested-layer fact,
trust decision, or allocation identity from today's workflow. An old session
whose context cannot be reconstructed remains stopped without caller-cwd
fallback. A retained declaration that uses a retired spelling and no longer
resolves has the named precondition failure `legacy declaration spelling is
unresolved`; migration does not rewrite it from current workflow declarations.

## Verify and recover

Run the release's configuration validation and inspect every workflow that has
agent actions, every population, each resource action input contract, and both
normal cleanup and explicit force-discard paths. Test a lifecycle-configuration
edit: it warns before the next execution and uses the current trusted
configuration. Test a vanished work directory: cleanup must fail its
precondition without changing directory. Keep the timestamped copies until
sessions have been recreated and the migrated state has been observed in normal
use.

## Recover a residual allocation

When a required definition, trusted configuration, recorded directory, cleanup
input, credential, environment, or reliable target identity is unavailable,
automatic reconstruction stops and reports operator recovery required. A
lifecycle-configuration warning alone does not have this outcome. Do not create
an empty directory or infer release from current configuration: neither proves
that a process or external allocation is gone.

1. Inspect the retained execution record, its unavailable reason, and the
   dependency-blocked executions.
2. Release the residual allocation through its external owner, following the
   retained plan's release order.
3. After the owner-approved acknowledgement operation is available, use
   `plect execution acknowledge-release --session <name> --execution <ulid>
   --reason <text>` for that exact execution. It records an auditable operator
   assertion, not successful cleanup, and does not release any other
   allocation. Until then, preserve the unresolved record or use the explicit
   discard in the next step.
4. Retry ordinary `up` after every applicable retained obligation is resolved;
   it reconstructs in the latest desired workflow's setup dependency order.

`--force-recreate` never substitutes for confirmation or discard, and recovery
never runs cleanup in another directory. `plect destroy --force` is the only
explicit discard: it records that release was not verified and removes the
session's remaining execution records.

To recover, stop the new release, restore the configuration and state copies,
and restart the previous release. Do not mix old configuration with migrated
state or migrated configuration with old state.
