# Tombstone Nodes/Tasks migration

This migration covers splitting a session tombstone's flat `tasks` map into
`nodes` (workflow-DAG node production records, including the `@workflow`
pseudo-node) and `tasks` (dynamic `plect task setup` instances), the same
split issue #461 gives the live `Session` type. See
[docs/design/sqlite-persistence.md](../design/sqlite-persistence.md) for the
live-store side of the same split — a tombstone is the one place this data
still lives as a plain JSON file rather than a SQLite row, since `plect
destroy` snapshots it into `events/<session>/tombstone.json` precisely so it
survives the row's own deletion.

The change is intentionally breaking. Plecture is pre-1.0, so operators
migrate a destroyed session's tombstone file once instead of the binary
carrying a permanent read path for the retired flat shape.

## Who this affects

- An operator with any `events/<session>/tombstone.json` file written by a
  plect build older than this migration (i.e. before the domain type gained
  `Nodes`/`Tasks` in place of one `Tasks` map with a per-entry `dynamic`
  flag). `plect status`/`plect ls` on a destroyed session reads this file.
- A session whose tombstone has no `tasks` entries at all is unaffected —
  there is nothing to split.

## Backup

Stop every running plect process against the data directory, then copy the
complete `events/` tree before editing anything:

```bash
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
BACKUP_DIR="$DATA_DIR/migration-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
cp -a "$DATA_DIR/events" "$BACKUP_DIR/events"
```

## State changes

Run every step under `set -euo pipefail`. The transform is per-file
(`events/<session>/tombstone.json`, one per destroyed session) and
idempotent: a file that already has a top-level `nodes` key is left
untouched, so re-running this procedure, or running it against a directory
where some tombstones already migrated, never re-splits an already-split
file. An entry with no `dynamic` field (the common case for a node, since
`false` was itself `omitempty`) is treated as a node — the same default the
retired field's own zero value gave it.

```bash
set -euo pipefail
FILTER='
  if has("nodes") then
    .
  else
    ((.tasks // {}) | with_entries(select(.value.dynamic != true) | .value |= del(.dynamic))) as $nodes
    | ((.tasks // {}) | with_entries(select(.value.dynamic == true) | .value |= del(.dynamic))) as $tasks
    | .nodes = $nodes
    | .tasks = $tasks
  end
'
find "$DATA_DIR/events" -mindepth 2 -maxdepth 2 -name tombstone.json | while IFS= read -r f; do
  jq "$FILTER" "$f" > "$f.new"
  mv "$f.new" "$f"
done
```

## Verification

For each migrated file, the multiset of instance keys must be unchanged
(only which of `nodes`/`tasks` a key lives under changed, and the
per-entry `dynamic` field is gone), and no entry may carry `dynamic` in
either collection:

```bash
find "$DATA_DIR/events" -mindepth 2 -maxdepth 2 -name tombstone.json | while IFS= read -r f; do
  BAD=$(jq '[(.nodes // {}), (.tasks // {})] | map(to_entries[]) | flatten
             | map(select(.value.dynamic != null)) | length' "$f")
  if [ "$BAD" -ne 0 ]; then
    echo "verification failed: $f still has $BAD dynamic-tagged entr(y/ies)" >&2
    exit 1
  fi
done
plect ls
plect status <a-destroyed-session-name>
```

`plect status` on a destroyed session with both a node and a dynamic
instance in its tombstone should show both in `work`, each with the correct
`dynamic` field in the JSON output (unchanged from before this migration —
only the on-disk storage shape changed, not the reported fact).

## Rollback

Stop plect processes, then restore the backed-up tree:

```bash
rm -rf "$DATA_DIR/events"
cp -a "$BACKUP_DIR/events" "$DATA_DIR/events"
```

Restart plect only after the restore is complete.

## Rollout note

This PR's own jq transform was run against a synthetic tombstone.json built
to match the pre-migration shape (a flat `tasks` map mixing a workflow node,
a run-scoped node, and a named dynamic instance) rather than a live
deployment file — none of this deployment's own destroyed sessions predate
the split. The transform correctly moved the two node entries into `nodes`,
kept the dynamic instance in `tasks` with `dynamic` removed, and produced
identical output on a second run (idempotency). `TestTombstoneStatusResult_
ReportsNodesAndTasksSeparately` (`app/internal/service/status_test.go`)
pins that a tombstone in the post-migration shape reports each instance's
`dynamic` field correctly.
