# SQLite persistence

This design implements [the SQLite durable-storage decision](../adr/2026-09-06-sqlite-durable-storage.md).

Core runtime persistence is one SQLite database at
`$XDG_DATA_HOME/plect/store.db`. The `app/internal/persistence` package owns
opening it, schema checks, the access gate, migrations, and translation between
database records and domain values. It opens every connection with WAL mode, a
non-zero bounded busy timeout, and foreign-key enforcement. Production opens
the file database; tests use a distinct temporary database file per test so
WAL, locking, and multiple connections exercise the production configuration.

`schema.sql` is the desired-structure authority. Reviewed goose migration SQL
is the historical transition authority, and the goose ledger in the same
database is the sole authority for applied versions. sqlc query results stay
inside the persistence package; service, Web, MCP, and command packages use
domain values rather than generated database types.

## Version authority and consumers

`contracts/state.SchemaVersion` is removed. It described a JSON envelope, not
the database or a service protocol. `contracts/state.StateFile` is also
removed: the package continues to provide concrete session, task, completion,
and tombstone values where app code needs those shared data shapes, but it no
longer promises a file readers can parse. The legacy migration plugin retains
its legacy-format version only inside the one-time importer.

`persistence.EnsureCurrent` replaces the live envelope check in each runtime
entry point. `app/commands/root.go` invokes it for every `plect` command;
`app/commands/serve.go` invokes it before accepting the bus socket; and
`app/internal/webui/live.go` invokes it while constructing the Web service.
The state and event-log packages become persistence-backed facades, so service
methods, the MCP server, dispatch, reactor, population supervisor, and session
hub receive an open persistence handle rather than independently interpreting
a versioned file. Only the importer reads a legacy state envelope.

## Durable records

The following tables preserve the present runtime data model. Identity,
relationship, ordering, and lookup values have columns and constraints. Fields
whose shape is defined by a workflow, hook, observer, or event producer remain
JSON in the record that owns them.

| Table | Key and relational columns | JSON or scalar payload | Source |
| --- | --- | --- | --- |
| `sessions` | `name` primary key; nullable `parent_session_name` and `root_session_name`, each referencing `sessions(name)`; `resource_id`, `alias`, `workflow`, `workspace_dir_path`, `created_at`, `updated_at` | conversation, message, inputs, health, channel-health, tick state, and other session fields | `state.json` `sessions` entries |
| `workflow_nodes` | `(session_name, node_id)` primary key; session foreign key; `scope`, `status`, `sequence` | task id, inputs, outputs, state, observed value, layers, lifecycle timestamps, error, done_when (rare, not relationally queried), and extra completion data | `state.json` `sessions.*.tasks` entries with `dynamic` unset |
| `task_instances` | `id` (ULID, minted fresh on every insert) primary key; `(session_name, instance_name)` unique; session foreign key; `task_id`, `scope`, `status`, `sequence`, `resource`, `named_instance` | inputs, outputs, state, observed value, layers, lifecycle timestamps, error, and extra completion data | `state.json` `sessions.*.tasks` entries with `dynamic: true` |
| `task_done_when_states` | `task_instance_id` primary key and foreign key | counters, fingerprints, reason/body, escalation data | `TaskState.DoneWhen` (dynamic instances only) |
| `task_done_when_judges` | `(task_instance_id, leaf_id)` primary key and task-instance foreign key; `judge_session`/`judge_workflow` remain stored facts | action, reason, revision, relation, and creation time | `DoneWhenState.Judges` (dynamic instances only) |
| `populations` | `population_key` primary key; `workflow`, `name` | none | `state.json` `populations` map key and value |
| `population_members` | `(population_key, resource_id)` primary key; nullable `session_name` | item, generation, timestamps, flags, decision, blockers | `PopulationState.Members` |
| `up_reservations` | `child_session_name` primary key; `parent_name`, `pid`, `reserved_at` | none | `state.json` `up_reservations` |
| `pending_deliveries` | `(session_name, resource_id, operation)` primary key; `operation` is subscribe or unsubscribe | none | `pending_delivery.json` |
| `event_streams` | `session_name` primary key; `generation` | none | each event directory and its `.gen` file |
| `events` | `event_id` primary key; `(session_name, sequence)` unique and references `event_streams` | type, source, direction, summary, body, metadata, delivery mode, and recorded time | each `log.jsonl` record |
| `event_consumer_positions` | `(session_name, consumer_name)` primary key and stream foreign key; `next_sequence` | none | `.cursor.<consumer>` |
| `event_watermarks` | `(session_name, watermark_name)` primary key and stream foreign key; `next_sequence` | none | `TickBackoff.LastLogPosition` |
| `session_tombstones` | `session_name` primary key; `destroyed_at` | tombstone session snapshot | `tombstone.json` |

`population_members.session_name` is a recorded fact, not an enforced foreign
key: admission can record a member's intended session name before that
session's own row exists.

`Session.Tasks` splits across `workflow_nodes` and `task_instances` by
whether the entry is a static workflow-DAG node (including the `@workflow`
pseudo-node) or a dynamic instance created at runtime via
`plect task setup`; the persistence layer reads both and composes the one
`Tasks` map the domain type and every core call site still see, deriving
`Dynamic` from which table a record came from rather than storing it.
`task_instances.id` is a ULID minted fresh on every insert, never reused
across a delete-and-reinsert, so a cleanup followed by a new setup under the
same `instance_name` is a distinct row with its own `task_done_when_states`/
`task_done_when_judges` history. Those two tables key off
`task_instances.id` and exist only for dynamic instances; a static workflow
node's `done_when` (declaring one is rare, and no shipped workflow node
relies on it) stays embedded in `workflow_nodes.record_json` instead of
being split out.

`task_done_when_judges` does not store the judged session/instance: the
owning `task_instances` row's own `(session_name, instance_name)` is always
the judged side (the one write path that records a verdict always stores it
on the same session/instance it names as the target), so
`DoneWhenJudge.TargetSession`/`Instance` are derived from that join at read
time rather than duplicated as columns.

`sessions` represents a real parent with `parent_session_name` and a
session-local pseudo-root with `root_session_name`; a check constraint permits
at most one. `children` is derived from those columns and is not stored. A
session may be deleted without deleting its event stream or tombstone, so
`event_streams` deliberately has no foreign key to `sessions`.

`events.sequence` is a positive, per-stream append position. It is not an
event identity and it is not a timestamp. A write transaction creates an
`event_streams` row when needed, assigns its generation, allocates the next
sequence, and inserts the event. The unique stream/sequence constraint gives
each log a total append order even when separate processes append concurrently.
Event IDs remain the global deduplication identity and the key used for merged
subtree ordering.

The database does not contain plugin-owned configuration or plugin-private
state. In particular, plugin catalog and lock files remain configuration, and
the Slack adapter's subscriber file remains adapter-owned data. These files
are not part of the runtime import.

## Schema excerpts

`app/internal/persistence/schema.sql` declares the application-owned tables.
The following excerpt shows the session relationship and event ordering shape;
the remaining tables follow the keys in the inventory above.

```sql
CREATE TABLE sessions (
    name TEXT PRIMARY KEY,
    parent_session_name TEXT REFERENCES sessions(name) ON DELETE SET NULL,
    root_session_name TEXT REFERENCES sessions(name) ON DELETE SET NULL,
    resource_id TEXT NOT NULL DEFAULT '',
    alias TEXT NOT NULL DEFAULT '',
    workflow TEXT NOT NULL DEFAULT '',
    workspace_dir_path TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    record_json TEXT NOT NULL,
    CHECK (NOT (parent_session_name IS NOT NULL AND root_session_name IS NOT NULL)),
    CHECK (root_session_name IS NULL OR root_session_name <> name)
);

CREATE INDEX sessions_alias_idx ON sessions(alias) WHERE alias <> '';
CREATE INDEX sessions_parent_idx ON sessions(parent_session_name);

CREATE TABLE event_streams (
    session_name TEXT PRIMARY KEY,
    generation TEXT NOT NULL
);

CREATE TABLE events (
    event_id TEXT PRIMARY KEY,
    session_name TEXT NOT NULL REFERENCES event_streams(session_name),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    recorded_at TEXT NOT NULL,
    type TEXT NOT NULL,
    source TEXT NOT NULL,
    direction TEXT NOT NULL,
    summary TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL,
    delivery_mode TEXT NOT NULL,
    UNIQUE (session_name, sequence)
);

CREATE INDEX events_stream_sequence_idx ON events(session_name, sequence);
CREATE INDEX events_stream_id_idx ON events(session_name, event_id);
```

The migration history begins with a transaction-only goose migration. Goose
records the migration version in its ledger in the same transaction as the
schema changes; migration files do not use a `NO TRANSACTION` directive.

```sql
-- +goose Up
CREATE TABLE event_streams (
    session_name TEXT PRIMARY KEY,
    generation TEXT NOT NULL
);

CREATE TABLE events (
    event_id TEXT PRIMARY KEY,
    session_name TEXT NOT NULL REFERENCES event_streams(session_name),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    recorded_at TEXT NOT NULL,
    type TEXT NOT NULL,
    source TEXT NOT NULL,
    direction TEXT NOT NULL,
    summary TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL,
    delivery_mode TEXT NOT NULL,
    UNIQUE (session_name, sequence)
);

CREATE INDEX events_stream_sequence_idx ON events(session_name, sequence);

-- +goose Down
DROP TABLE events;
DROP TABLE event_streams;
```

The append query obtains the next sequence inside the caller's write
transaction. The persistence append method creates a stream row first and
retries a transaction only when SQLite reports a busy conflict; it never
calculates a position outside the transaction.

```sql
-- name: NextEventSequence :one
SELECT COALESCE(MAX(sequence), 0) + 1
FROM events
WHERE session_name = ?;

-- name: InsertEvent :exec
INSERT INTO events (
    event_id, session_name, sequence, recorded_at, type, source, direction,
    summary, body, metadata_json, delivery_mode
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
```

## Transaction boundaries

The existing state store serializes `Put`, `Update`, `UpdatePopulation`,
`ReserveUpSlot`, `ReleaseUpSlot`, and `Delete` by loading and rewriting the
whole `state.json` file under `state.json.lock`
(`app/internal/state/store.go`). SQLite replaces each callback with the
following short transaction. No transaction contains an agent invocation,
hook, workspace action, network call, terminal operation, or channel delivery.
Every write transaction opens with `BEGIN IMMEDIATE` before reading rows. This
reserves a write-capable SQLite snapshot for read-then-write callbacks;
`busy_timeout` does not retry a deferred transaction's `SQLITE_BUSY_SNAPSHOT`
upgrade.

| Existing callback or append path | SQLite transaction | Behavioral contract |
| --- | --- | --- |
| `Put` | Upsert the session row, replace its `workflow_nodes`/`task_instances` rows (routed by `Dynamic`, each dynamic instance re-minting its `id`) and their completion rows, replace its population reference fields, and normalize the parent relation in one write transaction. | Preserves one durable checkpoint for the supplied session; it no longer rewrites unrelated sessions. |
| `Update` | Read the named session and its workflow-node/task-instance/completion rows, run the in-process callback, then write that session's changed rows in one write transaction. | Preserves read-modify-write atomicity and the missing-session error. The callback remains local and must not perform external work. |
| `UpdatePopulation` | Read or create one population and its members, run the callback, then replace that population's members in one write transaction. | Preserves an atomic population snapshot without serializing unrelated sessions. |
| `ReserveUpSlot` | Delete reservations whose recorded PID is no longer live, read the parent’s active children and reservations, enforce the cap, and insert the child reservation in one write transaction. | Preserves the live-holder rule and the rejection for an already-reserved child. |
| `ReleaseUpSlot` | Delete the named reservation in one write transaction. | Remains idempotent and best-effort at its existing call sites. |
| `Delete` | Delete the session, its tasks, completion state, reservations, and parent relation in one write transaction. | Preserves orphaning of children and preserves event history. Tombstone creation remains a separate step until the destroy path is deliberately made one database transaction. |
| `eventlog.Store.Append` | In one write transaction, create the stream if absent, read `NextEventSequence` from `MAX(sequence)`, and insert the event. | Preserves assigned ID/time, durable append, and per-session total order. |
| `CommitCursor` | Upsert one consumer’s next sequence in one write transaction. | Preserves at-least-once dispatch and reactor restart behavior. |
| `WriteTombstone` | Upsert one tombstone in one write transaction. | Preserves the fail-closed tombstone checkpoint. |

The present status-message path intentionally remains two operations:
`service.SetMessage` writes the session through `Put` and then appends a
status event (`app/internal/service/service.go`). A failed append continues to
report failure after the status write has committed. Lifecycle event recording,
judge-recorded events, task instruction events, terminal events, and
population notifications also remain independent best-effort appends where
their existing callers ignore append failures. This design does not introduce
event sourcing or claim that every state transition produces an event.

Destroy preserves its existing checkpoint order until its service contract is
changed: external cleanup, session checkpoint, tombstone write, lifecycle
append, then state deletion (`app/internal/service/lifecycle_destroy.go`). The
SQLite implementation keeps each database callback atomic but does not collapse
those independently observable milestones into a new all-or-nothing lifecycle
transaction.

## Migration access gate

SQLite's single-writer rule is insufficient: it does not make a running binary
aware that its schema assumptions are incompatible. `persistence` therefore
uses two local advisory lock files next to `store.db`:

- `store.db.access.lock` is the data-access gate. Every query and explicit
  transaction takes a shared lock for its duration. A migration takes its
  exclusive lock for the full migration.
- `store.db.coordination.lock` serializes migration intent. A normal access
  briefly takes a shared lock before taking the access gate. A migration takes
  it exclusively, writes and fsyncs `store.db.migration.json`, and retains it
  until the migration completes or fails.

The marker records only diagnostics: process ID, binary version, start time,
stage, and a failure message. It is not a version ledger. A process that sees
the coordination lock or a live marker waits for 30 seconds, then fails with an
actionable "migration in progress" error; it does not open the database. The
bound is `app/internal/persistence`'s fixed `migrationWait` constant, not a
configuration value. An access that passed the brief coordination check before
intent was recorded may obtain the shared access lock, so the migration waits
for that already-started operation. Once the migrator has the exclusive access
lock, no new normal operation can enter.

After it owns both exclusive locks, the runner opens the database, reads the
goose ledger again, and only then applies needed migrations. Each migration
file and its ledger entry commit atomically. A process interruption releases
the advisory locks; a later runner sees the stale marker, rechecks the ledger
under exclusion, replaces the marker, and resumes from the first unapplied
migration. A failed migration leaves its database transaction rolled back and
the marker with failure evidence. A later explicit retry replaces that marker
only after the same ledger recheck. No normal access initializes a replacement
database after an open, ledger, or migration error.

`plect storage migrate` runs this runner explicitly and reports the resulting
ledger version. Normal `plect` command startup, `plect serve`, and
`plect-web` call the same `EnsureCurrent` operation before serving work. A
database behind the embedded migration set is upgraded by the process that
first obtains migration exclusion; concurrent startup waits only for the
bounded period and otherwise refuses without accessing data. A database whose
goose ledger contains a version newer than the binary embeds always refuses
normal and migration access with an upgrade instruction and makes no change.

The resident service keeps pools but uses the persistence access gate around
each database operation, not around the process lifetime. Its HTTP event
routes answer an unavailable/retryable response while a migration blocks an
operation. `plect-web` performs the same startup check and its handlers use
the same facade. Plugin executables never open core's database; a plugin that
invokes `plect` or the local event service receives the same wait-or-refuse
result. This keeps provider code outside migration coordination and prevents a
second storage authority.

## Event positions and cursors

An event cursor is the opaque `event.Cursor{Off, Ord, Gen}` value, with
`CursorVersion` set to `2`. `Off` is the exclusive logical sequence in the
selected event stream: `1` starts at the first row, and a cursor after event
sequence `n` has `Off == n + 1`. `Gen` is `event_streams.generation`, and
`Ord` is the requested order.

`EventPage` decodes only version-2 cursors, validates `Ord` and `Gen` against
the selected stream, and reads `sequence >= Off` in ascending order. Its next
cursor has the exclusive position after the final scanned event. It returns an
empty page for a missing stream, preserves the current ascending and
non-paginating descending contracts, and rejects a stale generation or order
mismatch as invalid input.

`EventStreamResume` uses the same validation and position semantics. SSE
frames carry the encoded version-2 cursor as `id`; reconnect input carries that
opaque value. The JSON relay at `GET /api/v1/events/stream` validates its
`cursor` query value before it dials the bus and rejects a raw integer or a
version-1 token. The bus stream and the HTML relay at `GET /events/stream`
reject raw byte-offset `Last-Event-ID` values and version-1 tokens before
opening a stream. None parses a legacy byte offset as a sequence. The client
recovery path discards the stream cursor and refetches history, deduplicating
overlap by event ID as specified in [the event-history handoff
protocol](web-ui-event-history.md).

The subtree cursor also carries version 2 and keeps its event-ID keyset
position. `ListAcross` becomes a query over the selected stream names ordered
by event ID, so its merged ordering and root-scoped cursor behavior remain
unchanged. The bus and session hub read and append through this same event
store; neither retains a file-log implementation.

Server-held byte offsets translate during import. The importer records the
start and end byte boundary of every complete legacy JSONL line while assigning
event sequences. A sidecar cursor or `TickBackoff.LastLogPosition` must be
zero or an end boundary of a complete line; it becomes the sequence of the
next complete event, skipping malformed lines exactly as legacy `List` does.
An offset that is negative, beyond the complete log, inside a line, or inside a
trailing partial line rejects the import. Dispatcher and reactor positions go
to `event_consumer_positions`; the heartbeat position goes to
`event_watermarks` under its named watermark.

## One-time importer inventory

The import command runs only against an operator-created backup while writers
are stopped. It builds and validates a temporary database, validates it again,
then atomically promotes it as `store.db`. It never writes JSON and JSONL
alongside the database. The command reads the following runtime paths.

| Legacy path | Validation and destination |
| --- | --- |
| `state.json` | Parse once; require the supported legacy envelope version; validate layer effect identities and tree relationships; import sessions, tasks, completion state, populations, and reservations. |
| `state.json.lock` | Confirm it is not held before import; do not copy it. The access gate replaces it. |
| `events/<escaped-session>/log.jsonl` | Decode complete lines in byte order; reject invalid event identity, session mismatch, duplicate ID, and a malformed complete line; discard only a trailing partial line, matching the live reader; import stream and events. |
| `events/<escaped-session>/.gen` | Read one trimmed non-empty generation identifier when present; otherwise generate a new stream generation after recording that no old page cursor survives cutover. |
| `events/<escaped-session>/.cursor.<consumer>` | Parse a non-negative decimal boundary, validate it against the log boundary index, and import the translated consumer position. |
| `events/<escaped-session>/tombstone.json` | Decode one tombstone, require that its embedded name matches the directory session, and import its snapshot and destruction time. |
| `events/<escaped-session>/.lock` | Confirm it is not held before import; do not copy it. The database transaction and access gate replace it. |
| `pending_delivery.json` | Decode subscribe and unsubscribe maps; require non-empty session and resource values; deduplicate entries into `pending_deliveries`. |
| `pending_delivery.json.lock` | Confirm it is not held before import; do not copy it. |
| `delivery-locks/<escaped-session>.lock` | Confirm no lock is held; do not copy it. The delivery decision keeps its service-level serialization through a database-backed lock/transaction. |

The importer rejects unknown regular files inside a legacy event-session
directory and reports their paths; it does not silently discard a prospective
durable sidecar. Socket files, temporary files, and stale unlocked lock files
are reported but are not imported as data. Configuration files, plugin files,
workspaces, and adapter-private directories are outside this inventory.

After successful validation and database promotion, cutover writes the legacy
rejection marker at `state.json` as this envelope and removes no backup:

```json
{
  "version": 0,
  "sessions": {}
}
```

Version `0` is deliberately unsupported by every supported legacy binary, and
the marker contains no live data. The cutover verification starts a supported
legacy binary and exercises a mutation path; each must refuse before treating
the empty session map as a fresh store. Earlier binaries that lack this
version check are outside the supported upgrade path.

The migration procedure delivered with the importer uses this shape:

```md
# SQLite runtime persistence cutover

## Prerequisites

- Install a compiled core release that contains the SQLite driver.
- Stop every writer and confirm that no runtime lock is held.

## Backup

Copy the complete runtime data directory to a separate durable location and
verify the copy before continuing.

## Import and validation

Run `plect storage import --from <backup>`, inspect its validation report, and
promote only a clean temporary database.

## Cutover verification

Start the current binary, verify session and event reads, and verify that a
supported legacy binary rejects the marker on startup and mutation.

## Recovery

Stop writers, move the SQLite database aside for diagnosis, and restore the
backup. Restoring the backup discards updates made after cutover; downgrade is
not lossless.
```

## Layer ownership and alternatives

The database path resolution, connection settings, schema, query generation,
transactions, migration gate, importer, and cursor validation are core
responsibilities in `app/`. They provide durable identity, lifecycle,
relationships, observation, and handoff without naming a provider. The schema
contains no configuration syntax and adds no configuration key. Workspace
providers, channel adapters, and other plugins continue to provide external
effects and their own private persistence; they do not import the SQLite driver
through `contracts/`.

An advisory lock table alone is not selected because a process can use an old
schema before it reads that table. SQLite exclusive locking alone is not
selected because WAL readers and already-open processes still need an
application compatibility gate. A daemon-only store is not selected because
the local multi-process access model remains part of the runtime contract.
Keeping JSON/JSONL beside SQLite is not selected because it leaves two durable
authorities and prevents an atomic cutover. Fully normalizing task and event
payloads is not selected because workflow-defined values have no stable shared
schema; their identity and query boundaries remain relational while their
dynamic contents remain JSON.
