# record_json dissolution, session identity/lifecycle, and status message migration

This migration covers three changes that ship together (kecbigmt/plecture#465):

1. `sessions.record_json`, `node_instances.record_json`, and
   `task_instances.record_json` are removed. Every field they held gets a
   named column or a child table instead (`node_instance_layers`,
   `task_instance_layers`, `task_done_when_unsatisfied_items`,
   `population_member_blockers`, `session_channel_health`).
2. `sessions` gains a surrogate `id` (ULID) and a `status` enum
   (`down`/`up`/`destroyed`) with `destroyed_at`. `name` is now unique only
   among live (non-destroyed) rows. `plect destroy` no longer deletes the
   session row — it transitions `status` to `destroyed` and retains the row,
   its tasks, and its event history. The per-incarnation `event_streams`
   table is retired: `events`/`event_cursors` key off `sessions.id` directly,
   since a session row now *is* one incarnation.
3. `Session.Message` and `Session.Branch` are retired from `contracts/state`.
   A session's status line is derived from its most recent
   `plect.status_message` event (unchanged behavior for readers, since
   `service.SetMessage` already produced that event); a checked-out branch
   is read from the `@workflow` node's own provider-produced output instead
   of a session field.

The change is intentionally breaking. Plecture is pre-1.0, so operators
migrate their own database once instead of relying on a compatibility read.

## Who this affects

- An operator running plect with SQLite persistence
  (`$XDG_DATA_HOME/plect/storage.db`): every table above is rebuilt by an
  append-only migration that runs automatically the next time a plect binary
  built from this change opens the database.
- A script or dashboard reading `sessions`/`node_instances`/`task_instances`
  directly via `sqlite3` and expecting a `record_json` column, or an
  `event_streams` table, or `stream_id` columns on `events`/`event_cursors`.
- A script parsing `plect status --json`/`plect ls --json` output and
  expecting to find `branch` sourced from a stored session field rather than
  the workflow's own output (the JSON key and value are unchanged for a
  git-backed workflow; only the internal source moved).
- Any caller that destroyed a session and expected its name to disappear
  from `sqlite3 ... 'SELECT name FROM sessions'` entirely: it now stays,
  with `status = 'destroyed'`, and a same-name recreate adds a second row
  rather than reviving the first.

## Backup

Stop every running plect process and copy the database before running a
binary built from this change, since the schema rewrite is applied
automatically on open and rebuilds several tables in place:

```bash
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
BACKUP_DIR="$DATA_DIR/migration-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
cp "$DATA_DIR/storage.db" "$BACKUP_DIR/storage.db"
```

## What the migration preserves, and what it cannot

The migration backfills every relational identity/status column so
referential integrity holds and every session, task instance, and event
keeps working after upgrade:

- Each pre-existing session gets a surrogate `id`: reused from its latest
  `event_streams` row when it has one (so its existing events stay linked
  with no re-key), or freshly minted otherwise. `status` becomes `'down'`
  for every migrated (still-live) session.
- Every historical, already-superseded `event_streams` incarnation for a
  name becomes its own minimal `'destroyed'` session row (so its events
  keep a valid reference), with `workflow` set to `''` (unknown) since that
  fact no longer survives in the pre-migration database.
- `node_instances`/`task_instances` are re-keyed from `session_name` to the
  new `session_id`, preserving every row's identity, scope, status,
  sequence, and `finalized_at`.

The migration does **not** reconstruct the JSON payload fields
(`inputs`/`outputs`/`state`/`observed`/`layers`/`done_when`/error/lifecycle
timestamps) that used to live inside `record_json` into their new named
columns — every one of those reads as unset (`NULL`) on a row that predates
this migration. This SQLite persistence layer is itself only days old at
the time of this change (kecbigmt/plecture#453), predating the one-time
JSON/JSONL importer (kecbigmt/plecture#436) that will be the real,
carefully-mapped cutover from the legacy file-based state; a database
already running this interim schema is expected to hold disposable/recent
runtime state, not a permanent record worth a bespoke backfill. If a
pre-migration task instance's in-flight detail matters to you, capture it
(`plect status <session> --json`) before upgrading.

## Verification

```bash
grep -rniE 'record_json|event_streams|stream_id' --include='*.sh' --include='*.toml' --include='*.json' \
  ~/.config/plect .plect /path/to/your-scripts
```

The grep is authoritative for your own automation; it should return
nothing. Then confirm the migrated database:

```bash
sqlite3 "$DATA_DIR/storage.db" 'PRAGMA table_info(sessions);'         # id, status, destroyed_at present; no record_json
sqlite3 "$DATA_DIR/storage.db" 'PRAGMA table_info(events);'           # session_id, not stream_id
sqlite3 "$DATA_DIR/storage.db" "SELECT name, status FROM sessions;"   # every prior session reads status='down'
```

## Rollback

Stop plect, restore the backup, and run a plect binary built before this
change — the restored `storage.db` still has the pre-migration shape, which
a post-migration binary would otherwise have rewritten on next open:

```bash
cp "$BACKUP_DIR/storage.db" "$DATA_DIR/storage.db"
```

Restoring the backup discards any session/task/event activity recorded
between the backup and the rollback; it is not lossless.
