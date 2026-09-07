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

## Ordering relative to a binary update

Every `events/<session>/tombstone.json` a plect binary older than this
migration ever wrote has no top-level `nodes` key at all — the field did
not exist yet. A binary containing this migration's code, once it starts
handling `plect destroy` calls, can write a tombstone with a `tasks`-only
shape too (a session-scoped task document set up before any workflow node
ever ran is a legitimate, if unusual, case), and that shape is
indistinguishable from an old file by content alone: neither has a `nodes`
key, and the new format never stamps a per-entry marker to tell them apart.

The state-change step below relies on that absence being a reliable
"predates the split" signal, so it is one only if no new-format tombstone
can have been written yet. Stop every plect process (`plect serve`,
`plect-web`, any CLI invocation) against the data directory being migrated
**before** running the transform, and do not restart any of them — bringing
the updated binary back online — until the transform and its verification
below both complete. Under that ordering, every `tombstone.json` the
transform finds is guaranteed to predate the split, so `has("nodes")` is a
correct, total discriminator rather than a heuristic with an unresolvable
case — for *this* run. It is not a general-purpose "safe to re-run
anytime" property: once processes restart and a session with zero nodes is
destroyed, its tombstone is legitimately `tasks`-only with no `nodes` key
too, indistinguishable from an unmigrated one by content alone. A file the
`nodes`-key check skips is only skipped correctly because, at the moment
this procedure runs, nothing but this same procedure's own already-applied
work could have put it there. If the transform is interrupted partway
through the file list, re-stop any process that came back up and resume
this same run rather than starting a fresh one later against a directory
that may by then hold genuinely new `tasks`-only tombstones.

## Backup

With every plect process already stopped per the ordering requirement
above, copy the complete `events/` tree before editing anything:

```bash
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
BACKUP_DIR="$DATA_DIR/migration-backups/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUP_DIR"
cp -a "$DATA_DIR/events" "$BACKUP_DIR/events"
```

## State changes

Run every step under `set -euo pipefail`. The transform is per-file
(`events/<session>/tombstone.json`, one per destroyed session) and skips a
file that already has a `nodes` key — safe only under the ordering
requirement above, which guarantees such a file was never a pre-migration
one to begin with (either a prior, interrupted run of this same procedure
already split it, or every process capable of writing a fresh tombstone has
been stopped since before this run started). An entry with no `dynamic`
field defaults to a node — the same default the retired field's own zero
value gave it.

Per file, the instance-key set is captured before and after and compared —
migrating a session's declared entries into two collections must never
lose or duplicate one, and this check is what actually enforces that,
rather than assuming the transform below is total:

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
only the on-disk storage shape changed, not the reported fact). Restart
plect processes only once this step confirms `0` for every file.

## Rollback

Restore the backed-up tree (plect processes are still stopped, per the
ordering requirement — this migration never restarts them itself):

```bash
rm -rf "$DATA_DIR/events"
cp -a "$BACKUP_DIR/events" "$DATA_DIR/events"
```

## Rollout note

The transform and its key-set guard were run against synthetic fixtures
covering the cases correctness depends on: a pre-migration flat map mixing
a workflow node, a run-scoped node, and a named dynamic instance (split
correctly, keys preserved); a pre-migration flat map whose every entry is a
node and none is dynamic (split correctly into `nodes`, none left
misreporting `dynamic: true` — the case an earlier draft of this migration,
using a per-entry-`dynamic`-presence discriminator instead of the ordering
guarantee above, could not tell apart from new data and left unmigrated);
and an already-split file, standing in for the interrupted-and-resumed case
the `nodes`-key skip exists for (left byte-identical). `TestTombstoneStatusResult_ReportsNodesAndTasksSeparately`
and `TestTombstoneStatusResult_TasksOnlyShapeIsNotMistakenForLegacy`
(`app/internal/service/status_test.go`) pin that a tombstone in the
resulting shapes reports each instance's `dynamic` field correctly on the
Go read side.
