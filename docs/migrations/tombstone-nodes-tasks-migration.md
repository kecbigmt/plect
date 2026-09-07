# Tombstone Nodes/Tasks migration

This migration covers splitting a session tombstone's flat `tasks` map into
`nodes` (workflow-DAG node production records, including the `@workflow`
pseudo-node) and `tasks` (dynamic `plect task setup` instances), the same
split issue #461 gives the live `Session` type. A tombstone
(`events/<session>/tombstone.json`, written by `plect destroy`) is the one
place this data still lives as a plain JSON file rather than a SQLite row.

Plecture is pre-1.0, single-host, with no backward-compatibility path: this
one-time migration ships with a backup, and rollback is restoring that
backup and running the previous binary.

## Who this affects

An operator with any `events/<session>/tombstone.json` file written by a
plect build older than this migration. A session whose tombstone has no
`tasks` entries has nothing to split.

## Backup

Stop every plect process (`plect serve`, `plect-web`, any CLI invocation)
against the data directory, then copy the complete `events/` tree:

```bash
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
BACKUP_DIR="$DATA_DIR/migration-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
cp -a "$DATA_DIR/events" "$BACKUP_DIR/events"
```

Keep every plect process stopped until the transform and its verification
below both complete: an old tombstone has no top-level `nodes` key, and a
`tasks`-only tombstone this migration's own binary could write has none
either, so the transform below can only tell them apart by there being
nothing yet in the second category to confuse with the first.

## State changes

Run every step under `set -euo pipefail`. The transform is per file
(`events/<session>/tombstone.json`) and skips one that already has a
`nodes` key. An entry with no `dynamic` field defaults to a node — the
same default the retired field's own zero value gave it. Per file, the
instance-key set is captured before and after and compared, so the
transform can never lose or duplicate one silently:

```bash
set -euo pipefail
FILTER='
  if has("nodes") then
    .
  else
    ((.tasks // {}) | with_entries(select(.value.dynamic != true) | .value |= del(.dynamic))) as $found_nodes
    | ((.tasks // {}) | with_entries(select(.value.dynamic == true) | .value |= del(.dynamic))) as $found_tasks
    | .nodes = $found_nodes
    | .tasks = $found_tasks
  end
'
KEYSET='[(.nodes // {}), (.tasks // {})] | map(keys) | add | sort'
find "$DATA_DIR/events" -mindepth 2 -maxdepth 2 -name tombstone.json | while IFS= read -r f; do
  BEFORE=$(jq -S "$KEYSET" "$f")
  jq "$FILTER" "$f" > "$f.new"
  AFTER=$(jq -S "$KEYSET" "$f.new")
  if [ "$BEFORE" != "$AFTER" ]; then
    echo "migration verification failed: $f — instance keys changed, not replacing" >&2
    echo "before: $BEFORE" >&2
    echo "after:  $AFTER" >&2
    rm -f "$f.new"
    exit 1
  fi
  mv "$f.new" "$f"
done
```

## Verification

No entry may carry `dynamic` in either collection once every file above
has been replaced:

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
`dynamic` field in the JSON output. Restart plect processes only once this
step confirms `0` for every file.

## Rollback

Restore the backup at `$BACKUP_DIR` and run the previous binary.
