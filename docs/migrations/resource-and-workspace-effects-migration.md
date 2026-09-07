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

Migrate existing sessions to their recorded project root, initial layer
revisions and digest, and node execution records before running lifecycle
operations. An execution record retains cleanup definitions, setup inputs and
outputs, the execution directory, and resolved plugin version and reference,
including partial or failed setup. Preserve every executable and instruction
sidecar referenced by an unreleased record. An old session whose context cannot
be reconstructed is stopped and requires an operator-selected context after
backup; it must not fall back to a caller cwd.

## Verify and recover

Run the release's configuration validation and inspect every workflow that has
agent actions, every population, each resource action input contract, and both
normal and forced cleanup paths. Test a vanished work directory: cleanup must
fail without changing directory. Keep the timestamped copies until sessions
have been recreated and the migrated state has been observed in normal use.

## Recover a residual allocation

When recorded cleanup cannot run because its directory is gone, automatic
reconstruction stops and reports operator recovery required. Do not create an
empty directory as evidence of release: it does not prove that a process or
external allocation is gone.

1. Inspect the retained execution record, its cleanup information, and failure
   reason.
2. Release the residual allocation through its external owner.
3. Use the implementation's explicit operator-confirmation operation to record
   that assertion for that execution, including who confirmed it, what was
   released, when, and its audit event. This is not a successful cleanup and
   does not release any other allocation.
4. Retry ordinary `up` after every applicable retained obligation is resolved;
   it reconstructs under the retained release order.

`--force-recreate` never substitutes for confirmation, and recovery never
runs cleanup in another directory.

To recover, stop the new release, restore the configuration and state copies,
and restart the previous release. Do not mix old configuration with migrated
state or migrated configuration with old state.
