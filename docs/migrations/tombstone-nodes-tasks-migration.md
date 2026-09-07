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
(`events/<session>/tombstone.json`, one per destroyed session).

The pre-migration shape is discriminated by an explicit per-entry `dynamic`
key under `tasks`, not by the mere absence of a `nodes` key: a tombstone
already in the new shape but with only dynamic instances and zero nodes
ever produced (a legitimate, if unusual, shape — a session-scoped task
document can be set up before any workflow node ever runs) also has no
`nodes` key, and a bare `has("nodes")` check would wrongly reclassify its
already-correct `tasks` entries as nodes. `dynamic` is written only by the
pre-migration format — this build never writes it — so its presence
anywhere under `tasks` is unambiguous proof the file predates the split,
and its absence everywhere is proof the file needs no split, whether that
is because it is already migrated or because it is new. Only a pre-migration file whose every entry happens to be a workflow node
(never a dynamic instance, so `dynamic` was never `true` and, being
`omitempty`, never serialized at all) is indistinguishable from new data by
this signal. Such a file is left untouched rather than guessed at: it keeps
reporting every entry as `dynamic: true` (the flat map's own pre-migration
display bug, unrelated to this migration) until some other write touches
it, which is a narrow, display-only gap accepted deliberately rather than
risk corrupting a tombstone this build itself wrote correctly.

An entry with no `dynamic` field, in a file that does need the split,
defaults to a node — the same default the retired field's own zero value
gave it. The transform is naturally idempotent: once split, no entry in
either collection carries `dynamic` any more, so a second run's
`$needs_split` test is always false and every file is left as-is.

Per file, the instance-key set is captured before and after and compared —
migrating a session's declared entries into two collections must never
lose or duplicate one, and this check is what actually enforces that,
rather than assuming the transform below is total:

```bash
set -euo pipefail
FILTER='
  ((.tasks // {}) | to_entries | any(.value.dynamic != null)) as $needs_split
  | if $needs_split then
      ((.tasks // {}) | with_entries(select(.value.dynamic != true) | .value |= del(.dynamic))) as $found_nodes
      | ((.tasks // {}) | with_entries(select(.value.dynamic == true) | .value |= del(.dynamic))) as $found_tasks
      | .nodes = ((.nodes // {}) + $found_nodes)
      | .tasks = $found_tasks
    else
      .
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
has been replaced (the key-set guard already ran per file, inline, as part
of applying the transform — this checks the `dynamic` field is gone, the
other half of correctness):

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

The transform and its key-set guard were run against three synthetic
tombstone fixtures covering the cases correctness depends on: a
pre-migration flat map mixing a workflow node, a run-scoped node, and a
named dynamic instance (split correctly, keys preserved, idempotent on a
second run); an already-new-shape tombstone holding only dynamic instances
and no `nodes` key (left untouched — the case a `has("nodes")` discriminator
would have corrupted); and an already-new-shape tombstone with both
collections populated (left untouched). `TestTombstoneStatusResult_
ReportsNodesAndTasksSeparately` (`app/internal/service/status_test.go`)
pins that a tombstone in the post-migration shape reports each instance's
`dynamic` field correctly.
