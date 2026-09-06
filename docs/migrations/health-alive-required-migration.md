# Mandatory `[health.alive]` migration

This migration covers decision 1's configuration-language change from
[`../adr/2026-09-06-runtime-failure-model.md`](../adr/2026-09-06-runtime-failure-model.md):
every setup-bearing effect declares `[health.alive]`, and an explicit
non-observation is spelled as `type = "noop"`. Before this change, `[health]`
and `alive` inside it were both optional — see
[`../design/health-declaration.md`](../design/health-declaration.md) — so an
effect whose surface could vanish was free to declare no probe at all. The
loader now rejects that silence.

The change is intentionally breaking. Plecture is pre-1.0, so operators
migrate config once instead of relying on a compatibility shim. A
setup-bearing effect that still declares no `[health.alive]` fails to load
with `PLECTURE-CFG-HEALTH-ALIVE-REQUIRED`, naming the effect.

## Backup

Before editing any config directory, copy it to a timestamped backup:

```bash
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/plect"
BACKUP_DIR="$CONFIG_DIR/../plect-migration-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
cp -R "$CONFIG_DIR" "$BACKUP_DIR/config"
```

Do the same for a repo overlay's `.plect/` directory, if one carries effect
declarations of its own.

## Find affected effects

Every setup-bearing effect declaration needs `[health.alive]`. This finds a
definition file that declares `[<id>.setup]` without a matching
`[<id>.health.alive]`:

```bash
awk '
  match($0, /^\[([A-Za-z_][A-Za-z0-9_]*)\.setup\]$/, m) { setup[m[1]] = FILENAME }
  match($0, /^\[([A-Za-z_][A-Za-z0-9_]*)\.health\.alive\]$/, m) { alive[m[1]] = 1 }
  END { for (id in setup) if (!(id in alive)) print setup[id] ": " id }
' $(find "$CONFIG_DIR" .plect -name '*.toml' 2>/dev/null)
```

This also matches a `workspace_provider`, which declares `setup` too but is
outside this rule's scope (workspace providers have no `[health]` surface).
Check each match's `kind` field before editing it; only a `kind = "effect"`
match needs a change.

## Satisfy the rule

For each affected effect, add `[<id>.health.alive]` one of two ways:

**An executable probe**, when the effect's produced surface can be checked
for presence — a process, a socket, a generated wrapper file, a registered
subscription:

```toml
[my_effect.health.alive]
type   = "shell"
script = 'test -x "$dir/gh"'

[my_effect.health.alive.bind]
dir = { from = "self.outputs.dir" }
```

**`type = "noop"`**, when the effect deliberately never re-observes its
produced record — the produced entity may still exist, but Plecture chooses
not to check, because re-observing it wrongly (or recreating it after a
false negative) would be worse than trusting the record:

```toml
[my_effect.health.alive]
type = "noop"
```

`noop` is legal only under `[health.alive]`; the rule applies independently
to every layer of a nesting chain. See
[`../language/actions.md`](../language/actions.md#noop) and
[`../language/effects.md`](../language/effects.md#health).

## Verification

After migration:

```bash
plect ls
plect status <session> --refresh
```

`plect status` should show a HEALTH result driven by the probe just added —
`ok` for a passing executable check, or a session whose liveness silently
relied on the produced record for a `noop`-declared effect. No config file
should fail to load.

## Rollback

Restore the copied files:

```bash
rm -rf "$CONFIG_DIR"
cp -R "$BACKUP_DIR/config" "$CONFIG_DIR"
```
