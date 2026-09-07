# Mandatory workspace-provider `[health.alive]` migration

This migration covers
[`../adr/2026-09-06-vanished-workspace-provider-output.md`](../adr/2026-09-06-vanished-workspace-provider-output.md):
every workspace provider with `setup` — every workspace provider, since
`setup` is itself mandatory — declares `[health.alive]`, an executable probe
or an explicit `type = "noop"`. Before this change a workspace provider had
no `[health]` surface at all: a produced `@workflow` pseudo-node was reused
across `plect up` with no check that its recorded surface still existed.
`[health]` on a workspace provider admits `alive` only; `activity` remains an
effect-only field, since a provider has no scope and never joins the
periodic health cycle (see
[`../language/workspace-providers.md`](../language/workspace-providers.md#health)).

The change is intentionally breaking. Plecture is pre-1.0, so operators
migrate config once instead of relying on a compatibility shim. A workspace
provider that still declares no `[health.alive]` fails to load with
`PLECTURE-CFG-HEALTH-ALIVE-REQUIRED`, naming the provider; one that declares
`[health.activity]` fails to load with `PLECTURE-CFG-FIELD-UNKNOWN`.

This is a configuration migration, not a state migration: no session record
needs rewriting. An existing produced `@workflow` pseudo-node's stored
outputs supply the first `[health.alive]` evaluation the next `plect up`
performs, and a failed check follows the ordinary repair lifecycle (force
cleanup, then setup) rather than requiring any manual intervention.

## Backup

Before editing any config directory, copy it to a timestamped backup:

```bash
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/plect"
BACKUP_DIR="$CONFIG_DIR/../plect-migration-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
cp -R "$CONFIG_DIR" "$BACKUP_DIR/config"
```

Do the same for a repo overlay's `.plect/` directory, if one carries
workspace provider declarations of its own (workspace providers are trusted
base layers only, so this is rare, but check).

## Find affected providers

Every workspace provider declaration needs `[health.alive]`. This finds a
definition file that declares `[<id>]` with `kind = "workspace_provider"` but
no matching `[<id>.health.alive]`:

```bash
awk '
  match($0, /^\[([A-Za-z_][A-Za-z0-9_]*)\]$/, m) { id = m[1] }
  /kind[[:space:]]*=[[:space:]]*"workspace_provider"/ { provider[id] = FILENAME }
  match($0, /^\[([A-Za-z_][A-Za-z0-9_]*)\.health\.alive\]$/, m) { alive[m[1]] = 1 }
  END { for (id in provider) if (!(id in alive)) print provider[id] ": " id }
' $(find "$CONFIG_DIR" .plect -name '*.toml' 2>/dev/null)
```

The three shipped catalog providers (GitHub's `worktree`, Slack's
`thread_workspace`, the local-OKF plugin's `local_okf`) already declare
`[health.alive]` as of this change; this only finds a host- or
plugin-authored provider that predates it.

## Satisfy the rule

For each affected provider, add `[<id>.health.alive]` one of two ways:

**An executable probe**, when the workspace surface can be checked for
presence — most commonly, whether `workspace_dir` still exists:

```toml
[my_provider.health.alive]
type   = "shell"
script = 'test -d "$workspace_dir"'

[my_provider.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }
```

A richer surface can check more than a bare directory. The shipped
`worktree` provider also confirms git still recognizes the directory as a
worktree:

```toml
[worktree.health.alive]
type   = "shell"
script = 'test -d "$workspace_dir" && git -C "$workspace_dir" rev-parse --git-dir >/dev/null'

[worktree.health.alive.bind]
workspace_dir = { from = "self.outputs.workspace_dir" }
```

`[health.alive]` may read `self.outputs.<key>` only for a key the provider's
own `outputs_schema` declares — the same rule `cleanup` follows — plus
`inputs.<key>`, `session.name`, and `config.workspace_dirs_root`. It may not
read `cleanup.inputs.<key>` or `force`: those belong to an explicit teardown,
not a liveness check.

**`type = "noop"`**, when the provider deliberately never re-observes its
produced record:

```toml
[my_provider.health.alive]
type = "noop"
```

A `noop` provider keeps today's behavior exactly: `plect up` reuses the
produced pseudo-node without executing anything, the same as before this
migration.

## Verification

After migration:

```bash
plect up <session>
```

For an executable probe, remove (or rename) the session's `workspace_dir`
directory first and confirm `plect up` recreates it: the alive check fails,
provider cleanup runs with `force = true`, setup runs again, and any
produced node whose input binds `workflow.outputs.*` or `workspace.*` is
rebuilt. For an intact workspace, confirm an ordinary `plect up` leaves
uncommitted work in the workspace untouched and does not run provider
cleanup.

## Rollback

Restore the copied files:

```bash
rm -rf "$CONFIG_DIR"
cp -R "$BACKUP_DIR/config" "$CONFIG_DIR"
```
