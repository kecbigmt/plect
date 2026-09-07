# `event.DeliveryMode` retirement migration

This migration covers retiring `contracts/event.Event.DeliveryMode`, its
`Filter` counterpart, and the `events.delivery_mode` SQLite column. Nothing
in core ever branched on the stored value — no dispatcher, reactor, channel
filter, or bus path read it to decide behavior — and it was fully derivable
from `type` carrying the `plect.terminal.` prefix
(`contracts/event.TypeTerminalPrefix`). `plect event list`, the Web API's
`EventPage` projection, and (unaffected by this change) the CLI's own
DELIVERY column now derive push/pull from that prefix instead of reading a
stored field.

The change is intentionally breaking. Plecture is pre-1.0, so operators and
automation migrate their own usage once instead of relying on a
compatibility read. A caller still naming `delivery_mode` on a publish path
(`plect event publish`, the `plect_event_publish` MCP tool, or a raw
`POST /v1/events` body) now fails loudly with an actionable error, rather
than having the field silently ignored and the caller believe its delivery
preference was honored.

## Who this affects

- A script or dashboard calling `plect event list --delivery-mode <push|pull>`
  or `plect event tail --delivery-mode <push|pull>` — the flag is gone from
  both commands.
- An MCP client passing `delivery_mode` to the `plect_event_list` tool — the
  parameter is gone.
- A client of the event bus HTTP API passing `delivery_mode` as a
  `GET /v1/events` or `GET /v1/stream` query parameter — it is no longer
  read (silently has no effect, matching any other unrecognized query key on
  those two read paths).
- Any of the above naming `delivery_mode` on a *publish* path — `plect event
  publish`, the `plect_event_publish` MCP tool, or a raw `POST /v1/events`
  body — now gets a 400 / tool-error response naming the field, not a
  silently-dropped write.
- An operator running plect with SQLite persistence enabled
  (`$XDG_DATA_HOME/plect/store.db`): the `events.delivery_mode` column is
  dropped by an append-only migration that runs automatically the next time
  a plect binary built from this change opens the database.

## Backup

SQLite persistence is new (the schema shipped in the immediately preceding
change); if you have not opted into it, there is nothing to back up here.
If you have, stop every running plect process and copy the database before
running a binary built from this change, since the column drop is applied
automatically on open and rebuilds the `events` table in place:

```bash
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
BACKUP_DIR="$DATA_DIR/migration-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
cp "$DATA_DIR/store.db" "$BACKUP_DIR/store.db"
```

## Replace `--delivery-mode` filtering

`push` meant exactly "a `plect.terminal.*` event" (done/escalate/dead); every
other event was `pull`. Use a type glob instead:

```bash
# before
plect event list <session> --delivery-mode push

# after
plect event list <session> --type "plect.terminal.*"
```

There is no single-flag equivalent for `--delivery-mode pull` (a type-glob
`Filter.Types` is an include list, not an exclude list). Filter the JSON
output instead:

```bash
plect event list <session> --json \
  | jq '.events |= map(select(.type | startswith("plect.terminal.") | not))'
```

The same substitution applies to `plect event tail` and to the
`plect_event_list` MCP tool's `types` parameter.

## Replace a bus `delivery_mode` query parameter

A `GET /v1/events` or `GET /v1/stream` caller passing `delivery_mode=push`
gets back an unfiltered (or `types`-filtered, if also set) response now that
the parameter is ignored. Add `types=plect.terminal.*` to the query instead.

## Delete a `delivery_mode` publish input

Drop the field entirely; it never controlled anything plect read back:

```diff
- plect event publish <session> --type user.emit --delivery push
+ plect event publish <session> --type user.emit
```

```diff
  {
    "session_name": "...",
    "type": "user.emit",
-   "delivery_mode": "push"
  }
```

## Verification

```bash
grep -rniE 'delivery_mode|DeliveryMode' --include='*.sh' --include='*.toml' --include='*.json' \
  ~/.config/plect .plect /path/to/your-scripts
```

The grep is authoritative for your own automation; it should return nothing.
Then confirm the migrated database and CLI:

```bash
plect event list <session> --type "plect.terminal.*"
sqlite3 "$DATA_DIR/store.db" 'PRAGMA table_info(events);'   # no delivery_mode row
```

## Rollback

Stop plect, restore the backup, and run a plect binary built before this
change — the restored `store.db` still has `events.delivery_mode`, which a
post-migration binary would otherwise have dropped on next open:

```bash
cp "$BACKUP_DIR/store.db" "$DATA_DIR/store.db"
```
