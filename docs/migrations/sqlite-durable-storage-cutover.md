# SQLite durable storage cutover

This migration covers the one-time transition from JSON/JSONL runtime
persistence to SQLite (kecbigmt/plecture#436), implementing
[the SQLite durable-storage decision](../adr/2026-09-06-sqlite-durable-storage.md)
and [its design](../design/sqlite-persistence.md). It is a one-time,
operator-run procedure, not an automatic migration a plect binary applies on
open: the source format (`state.json`, per-session
`events/<session>/log.jsonl` and sidecars) has no schema-version field of
its own to detect, and importing it requires every writer stopped first.

The change is intentionally breaking. Plecture is pre-1.0, so operators run
this procedure once instead of relying on a compatibility read of the old
files.

## Who this affects

- An operator upgrading a plect installation that has never run a build with
  SQLite persistence before (any build older than kecbigmt/plecture#433).
- Every later release always requires SQLite: there is no configuration
  flag to keep using the old file layout going forward.

## Prerequisites

- Install a compiled `plect` release that contains the SQLite driver
  (`go install github.com/kecbigmt/plecture/app/cmd/plect@latest`, or the
  release pipeline's packaged binary once kecbigmt/plecture#429 ships).
- Stop every running plect process against the data directory being
  migrated: `plect serve`, `plect-web`, and any `plect` CLI invocation.
  Confirm nothing still holds `state.json.lock`, `pending_delivery.json.lock`,
  `delivery-locks/*.lock`, or any `events/<session>/.lock` — `plect storage
  import` itself refuses if any of these is still held, but it does not stop
  a writer on your behalf.

## Backup

Copy the complete runtime data directory to a separate durable location
before running anything else, and keep that copy until the new database is
verified:

```bash
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
BACKUP_DIR="$HOME/plect-legacy-backup-$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$DATA_DIR" "$BACKUP_DIR"
```

`$DATA_DIR` at this point holds only the legacy layout (`state.json`, the
`events/` tree, `pending_delivery.json`, `delivery-locks/`) — no
`storage.db` yet, since this build has never opened one against it.

## Import and validation

Run the importer against the **backup**, not the live directory, so an
interrupted or rejected attempt never touches production data:

```bash
plect storage import --from "$BACKUP_DIR" --dry-run
```

Inspect the printed report (session, event, cursor, population, and
reservation counts, plus any files it did not recognize). A dry run never
writes anything; re-run it after fixing any reported problem. Once the
report is clean, run the real import:

```bash
plect storage import --from "$BACKUP_DIR"
```

This builds a temporary SQLite database from the backup, validates it twice
(SQLite's own `PRAGMA integrity_check`, then a re-read confirming every
intended session and event is present), and only then:

1. Overwrites `$DATA_DIR/state.json` with the legacy rejection marker
   (`{"version":0,"sessions":{}}`) — a legacy binary that only understands
   the old envelope refuses to start or mutate against version `0` rather
   than treating an empty session map as a fresh store.
2. Atomically renames the temporary database into place as
   `$DATA_DIR/storage.db`.

The marker is written first deliberately: the only step left that can still
fail afterward is the rename, which leaves no `storage.db` at `$DATA_DIR` —
so a legacy binary is already locked out, never a working new database
sitting next to an unmodified legacy `state.json` that a legacy binary would
keep trusting. Either way, re-running the same command against the same
backup finishes the job (the marker overwrite is idempotent, and the
missing `storage.db` never trips the "already exists" refusal).

`tombstone.json`, `chain_attempts.json`, `pending_delivery.json`,
`delivery-locks/`, and their lock files are **not** imported into SQLite —
they stay exactly where they are in `$DATA_DIR` and remain file-based after
cutover (see `app/internal/persistence/schema.sql`'s own comment). Only
`state.json` and the `events/` tree feed the database.

## Cutover verification

Start the current binary and verify session and event reads
(`plect ls --all`, `plect event list <session>`) look correct against the
backup's contents. In particular, confirm `plect ls`'s session count equals
the backup's `state.json` session count (`jq '.sessions | length'
"$BACKUP_DIR/state.json"`): every `events/`-only session imports as
destroyed and stays hidden from the listing, so the two must match exactly,
not merely be close. Then verify that a supported legacy binary — one built
before kecbigmt/plecture#433 (commit `747768e`) — refuses against the marker
rather than starting fresh, on both a read/startup path and a mutation path:

```bash
plect-legacy ls               # any command; refuses at startup, before touching anything
plect-legacy up <resource-id> # a mutation path; refuses the same way
```

Verified against the last pre-cutover commit (`8bac9c3`, immediately before
`747768e`) with a scratch data directory holding only the marker: both `ls`
and `up` exit 1 with the error

```
state schema version mismatch: got 0, want 7; run `go run ./plugins/legacy-migration/cmd/legacy-migration` before using this plect binary
```

and `state.json`'s content is byte-for-byte unchanged afterward (only its
`.lock` sidecar appears, which every read already creates). `plect` command
startup calls `state.Store.CheckReadable` — which
propagates this same error — before any command body runs, so every
subcommand refuses this way, not only `ls`/`up`. A handful of no-error-return
read accessors on that legacy `state.Store` (`Get`/`All`/`FindByAlias`, as
opposed to their `GetE`/`AllE`/`FindByAliasE` counterparts) degrade a load
error to an empty result instead of surfacing it; none of them run at
startup or on a mutation path, so they cannot silently resurrect state, but
a legacy binary that predates the `...E` split entirely (older than the
already-present dual API at commit `8bac9c3`) is outside the upgrade path
this procedure verifies.

Client-held pagination and stream-resume cursors from before the cutover are
invalidated, not translated: `contracts/event.CursorVersion` was already
bumped to 2 as part of kecbigmt/plecture#434, before this cutover shipped.
A browser tab left open across the upgrade discards its stale cursor and
refetches history on reconnect, per
[the event-history recovery contract](../design/web-ui-event-history.md).

## What the importer preserves, and what it does not

- Session identity, parent/root relationships, tasks (static and dynamic),
  layers, `done_when`/judge state, populations, and up-slot reservations —
  every field `state.json` carried, translated by the same code path that
  reads a live `domain.Session`.
- Event ordering and content: `events/<session>/log.jsonl` imports in byte
  order, each record keeping its original id, time, type, source,
  direction, summary, body, and metadata. A record with no `direction`
  (predating that field) imports as `internal`, counted in the report.
- A session's SQLite identity: reused from `events/<session>/.gen` when
  present, so its imported events stay linked to the same row; freshly
  minted otherwise.
- Consumer progress: `.cursor.dispatcher`/`.cursor.tick-reactor` byte
  offsets translate into `event_cursors` as the `delivery`/`tick` kinds; a
  session's legacy `tick_backoff.last_log_position` translates into the
  `heartbeat` kind the same way. Each byte offset must be `0` or the exact
  end boundary of a complete `log.jsonl` line — a stray or corrupted offset
  fails the import rather than being reinterpreted as a nearby valid one.
- A session whose only trace is an `events/<session>` directory imports as
  `status = "destroyed"`, `destroyed_at` set to its last event's time (or the
  import time itself, for the degenerate case of an empty log): the legacy
  store deleted a destroyed session's `state.json` entry but kept its event
  log, so this is what an `events/`-only directory almost always means.
  Its history stays readable (`plect event list <session>`); it is simply
  hidden from `plect ls` and excluded from reconcile, like any other
  destroyed session. A host that already ran an older importer version
  before this rule shipped needs the one-time
  `plect storage repair-imported-sessions --from <legacy backup dir>` fix
  below.

The importer does **not** reconstruct a legacy session's `up`/`down`
liveness: `state.json` predates the `status` column entirely (see
[the record_json dissolution migration](record-json-dissolution-and-session-lifecycle-migration.md)),
so every session `state.json` names imports `status = "down"` regardless of whether it
was actually running before cutover. Run `plect up <session>` for any
session that needs to be live again; its tasks and history are unaffected.

## Repairing a host already imported

A host that ran `plect storage import` with an importer version older than
the fix that made an `events/`-only session import as destroyed left every
such session as a ghost `status = "down"` row instead: live-but-inert
entries that inflate `plect ls` and every reconcile tick (see
kecbigmt/plecture#507). `plect storage repair-imported-sessions` is a
one-time, single-host fix for that host's already-promoted `storage.db`; it
is not a standing migration path and can be removed once every host that
needs it has run it, or once v0.3.0 ships, whichever comes first.

It takes the same `--from <legacy backup dir>` the original import used:

```bash
plect storage repair-imported-sessions --from "$BACKUP_DIR" --dry-run
```

Inspect the printed counts (`would-mark` / `already-destroyed` / `kept`).
`would-mark` is the `events/`-only sessions this run would retire;
`already-destroyed` is any of those a prior repair run (or a manual fix)
already retired; `kept` is every session `--from`'s `state.json` also
names — left untouched either way. A dry run writes nothing. Once the
report looks right, run it for real:

```bash
plect storage repair-imported-sessions --from "$BACKUP_DIR"
```

This takes a dated backup of `storage.db` (plus its `-wal`/`-shm` siblings,
if present) at `--data-home` before marking anything, then transitions each
`would-mark` session to `status = "destroyed"` with `destroyed_at` set the
same way the importer sets it (its last event's time). Afterward, `plect
ls`'s session count should equal `--from`'s `state.json` session count, the
same invariant the "Cutover verification" section above checks for a fresh
import.

## Recovery

Stop every plect process, move the SQLite database aside for diagnosis, and
restore the backup made above:

```bash
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/plect"
mv "$DATA_DIR/storage.db" "$DATA_DIR/storage.db.broken-$(date -u +%Y%m%dT%H%M%SZ)"
rm -f "$DATA_DIR/storage.db-wal" "$DATA_DIR/storage.db-shm"
cp -a "$BACKUP_DIR/." "$DATA_DIR/"
```

Restoring the backup discards any update made after cutover (new sessions,
events, task transitions); downgrade is not lossless. There is no supported
path back to the legacy file layout for a database that has already
accepted writes.
