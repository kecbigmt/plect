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
STATE_ROOT="${PLECT_STATE_HOME:-$HOME/.local/state/plect}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$CONFIG_ROOT" "$CONFIG_ROOT.resource-effects-backup.$STAMP"
cp -a "$STATE_ROOT" "$STATE_ROOT.resource-effects-backup.$STAMP"
```

For each project root, copy its `.plect/` tree to a sibling backup before
placing a project marker or consolidating configuration.

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
only that root's intended `.plect/` definitions, and add
`.plect/project.toml`. Add the root's canonical path to
`trusted_project_roots` in the machine configuration. Do not preserve ancestor
fragments or copy configuration from a generated checkout.

Migrate existing sessions to their recorded project root, layer revisions, and
effective digest before running lifecycle operations. An old session whose
context cannot be reconstructed is stopped and requires an operator-selected
context after backup; it must not fall back to a caller cwd.

## Verify and recover

Run the release's configuration validation and inspect every workflow that has
agent actions, every population, each resource action input contract, and both
normal and forced cleanup paths. Test a vanished work directory: cleanup must
fail without changing directory. Keep the timestamped copies until sessions
have been recreated and the migrated state has been observed in normal use.

To recover, stop the new release, restore the configuration and state copies,
and restart the previous release. Do not mix old configuration with migrated
state or migrated configuration with old state.
