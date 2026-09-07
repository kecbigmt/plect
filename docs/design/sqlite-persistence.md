# SQLite persistence

This design implements [the SQLite durable-storage decision](../adr/2026-09-06-sqlite-durable-storage.md).

Core runtime persistence is one SQLite database at
`$XDG_DATA_HOME/plect/storage.db`. The `app/internal/persistence` package owns
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
JSON in the record that owns them. There is no `record_json` grab-bag column
on any table: every field a table's row carries either has a named column (or
a child table) or does not exist as a stored fact.

A column is `NULL` exactly when the domain value it holds can be genuinely
absent — never observed or resolved yet, or an optional fact. A column that
domain logic always populates is `NOT NULL` with no `DEFAULT`, so every write
site states its value explicitly rather than silently inheriting one from the
database. A closed-set text column (`scope`, `status`, an action or relation,
a decision kind) carries a `CHECK (col IN (...))` naming its exact values; a
boolean column is `boolean NOT NULL CHECK (col IN (0, 1))` rather than SQLite's
untyped affinity alone. Every `*_at` timestamp column is a UTC RFC3339 string
with exactly nine fractional digits (for example
`2026-09-06T08:50:42.821423717Z`), or `NULL` when unset — never a
variable-width fractional part, so lexical order equals time order; see
`timeconv.go`.

A text column holding JSON carries the `_json` suffix. Every `_json` column
is declaration-owned: its shape comes from a configuration-language or
provider schema, and the database stores it opaquely, enforcing only
well-formedness (`CHECK (col IS NULL OR json_valid(col))`, or `CHECK
(json_valid(col))` for a `NOT NULL` one like `events.metadata_json`), never
its internal structure. Core-owned structure is never stored as JSON — it
gets relational columns or a child table instead. Two narrow exceptions
exist, each for the same reason: the shape is rare enough, and never
relationally queried, that splitting it into columns/child tables would add
relational structure with no query that uses it.

- `node_executions.done_when_json`: a static workflow node's `done_when` is
  core-owned shape, but declaring one is rare (no shipped workflow node
  relies on it), so it is never split into the
  `task_done_when_states`/`task_done_when_judges` shape `task_instances`
  gets.
- `node_executions.cleanup_json` and `node_execution_layers.cleanup_json`:
  the retained cleanup contract (see "Node execution identity" below) is a
  snapshot of a `lang.Action` plus its ownership/joint facts — core-owned
  Go structs (`task.RetainedCleanup`, `effect.RetainedLayerCleanup`), not a
  provider schema — but it exists purely to be replayed at release time,
  never queried by column.

These are one-off carve-outs for that reason alone, not a general license
for core-owned structure to hide in a `_json` column. The table below
classifies every `_json` column by its owning declaration:

| Column | Owning declaration |
| --- | --- |
| `sessions.inputs_json` | the session's `inputs_schema` (config language) |
| `node_executions`/`task_instances`.`inputs_json`, `.outputs_json` | the task/effect's `inputs_schema`/`outputs_schema` |
| `node_executions`/`task_instances`.`state_json` | consumer-defined keys a reviewer or another session records; no declared schema |
| `node_executions`/`task_instances`.`resource_observation_json` | the resource observer's `state_schema` |
| `node_executions`/`task_instances`.`extra_done_when_json` | the config-language `done_when` schema (a `--done-when-json` instance override; see `service.judge.go`'s `effectiveDoneWhen`) |
| `node_executions.done_when_json` | core-owned shape, embedded as a narrow, documented exception (see above) rather than declaration-owned |
| `node_executions.cleanup_json`, `node_execution_layers.cleanup_json` | core-owned shape (`task.RetainedCleanup`/`effect.RetainedLayerCleanup`), embedded as a narrow, documented exception (see above) rather than declaration-owned |
| `node_execution_layers`/`task_instance_layers`.`inputs_json`, `.locals_json`, `.outputs_json` | the nesting layer's own effect declaration |
| `node_execution_layers`/`task_instance_layers`.`env_json` | the workflow/effect's declared process environment for that layer |
| `population_members.item_json` | the workspace provider's own resource-item map |
| `events.metadata_json` | the event producer's own map (a provider's change-type payload, a channel's delivery detail, ...) |

| Table | Key and relational columns | JSON or scalar payload | Source |
| --- | --- | --- | --- |
| `sessions` | `id` (ULID) primary key; `name` (unique only among live rows — see "Session identity and lifecycle"); `status`, nullable `destroyed_at`; nullable `parent_session_id` and `root_session_id`, each referencing `sessions(id)`; nullable `resource_id`, `alias`, `workspace_dir`, `population_workflow`, `population_name` (the pair also references `populations(workflow, name)`); `workflow`, `created_at`, `updated_at` | inputs, health, tick backoff | `state.json` `sessions` entries |
| `node_instances` | `(session_id, node_id)` primary key; session foreign key; no other columns | none — purely the logical node's identity | `state.json` `sessions.*.tasks` entries with `dynamic` unset (keys only) |
| `node_executions` | `id` (ULID, minted per setup attempt) primary key; `(session_id, node_id)` foreign key to `node_instances`; `sequence`; nullable `task_id`, `name`, `resource`; `scope`, `status`, nullable `finalized_at`; at most one unreleased (`status <> 'cleaned'`) row per `(session_id, node_id)` — see "Node execution identity" | inputs, outputs, state, observed value, `done_when` (rare, not relationally queried), extra completion data, retained cleanup contract, execution directory, plugin reference, error, lifecycle timestamps | `state.json` `sessions.*.tasks` entries with `dynamic` unset (per-attempt facts) |
| `node_execution_layers` | `(execution_id, position)` primary key; execution foreign key (`ON DELETE CASCADE`) | inputs, locals, outputs, env, heartbeat counters, lifecycle timestamps, error, retained per-layer cleanup contract | `TaskState.Layers` (static node instances) |
| `node_execution_dependencies` | `(execution_id, depends_on_execution_id)` primary key; both reference `node_executions(id)` (`ON DELETE CASCADE`) | none — the edge itself is the payload | snapshotted from `task.Resolved.DependsOn` at the dependent's own setup time — see "Node execution identity" |
| `task_instances` | `id` (ULID, stable across every write that still names the same `(session_id, instance_name)`; re-minted only when a cleanup removes the row before a later setup recreates it) primary key; `(session_id, instance_name)` unique; session foreign key; `task_id`, `scope`, `status`, `sequence`, nullable `resource`, `named`, nullable `finalized_at` | inputs, outputs, state, observed value, extra completion data, error, lifecycle timestamps | `state.json` `sessions.*.tasks` entries with `dynamic: true` |
| `task_instance_layers` | `(task_instance_id, position)` primary key; task-instance foreign key | inputs, locals, outputs, env, heartbeat counters, lifecycle timestamps, error | `TaskState.Layers` (dynamic instances) |
| `task_done_when_states` | `task_instance_id` primary key and foreign key | counters, fingerprints, reason/body, escalation data | `TaskState.DoneWhen` (dynamic instances only) |
| `task_done_when_unsatisfied_items` | `(task_instance_id, position)` primary key; task-instance foreign key | one unsatisfied-leaf id per row | `DoneWhenState.LastUnsatisfied` |
| `task_done_when_judges` | `(task_instance_id, leaf_id)` primary key and task-instance foreign key; `judge_session`/nullable `judge_workflow` remain stored facts | action, reason, revision, relation, and creation time | `DoneWhenState.Judges` (dynamic instances only) |
| `populations` | `(workflow, name)` primary key | none | `state.json` `populations` map key and value |
| `population_members` | `(workflow, name, resource_id)` primary key and `populations(workflow, name)` foreign key; nullable `session_name` | item, generation, timestamps, flags, `decision_kind`/`decision_reason` | `PopulationState.Members` |
| `population_member_blockers` | `(workflow, name, resource_id, position)` primary key; foreign key to `population_members` | one blocker reason per row | `PopulationMember.LastBlockers` |
| `session_channel_health` | `(session_id, kind)` primary key; session foreign key; `kind` CHECK IN `validation`/`delivery` | consecutive failures, first/last failure time, last channel/error, escalation time | `Session.ChannelValidationHealth`/`ChannelDeliveryHealth` |
| `up_reservations` | `child_session_name` primary key; nullable `parent_session_name`, `virtual_root`, `pid`, `reserved_at` | none | `state.json` `up_reservations` |
| `events` | `id` primary key; `(session_id, sequence)` unique and references `sessions(id)`; `direction` CHECK IN `inbound`/`outbound`/`internal`; `metadata_json` CHECK `json_valid` | type, source, direction, summary, body, metadata, and recorded time | each `log.jsonl` record |
| `event_cursors` | `(session_id, kind)` primary key and session foreign key (`ON DELETE CASCADE`); `kind` CHECK IN `delivery`/`tick`/`heartbeat`; `next_sequence` | none | `.cursor.<consumer>` |

### Stays file-based after cutover

Not every pre-cutover artifact moves into `storage.db`. Each of the
following remains exactly where it already lives, unmigrated by the
importer and untouched by cutover:

| File | Why it stays file-based |
| --- | --- |
| `events/<session>/tombstone.json` | A per-session snapshot written once, at destroy, and read rarely (post-teardown diagnostics); it needs no index, no relational query, and no transactional coupling with the tables it survives independently of. `eventlog.Store.WriteTombstone`/`ReadTombstone` read and write it directly. |
| `events/<session>/chain_attempts.json` | A best-effort, per-session cap-refusal dedup streak, not a durable historical fact — routinely swept and replaced wholesale, never queried across sessions. `eventlog.Store`'s `SwapChainAttempt`/`RevertChainAttempt`/`ClearChainAttempts` own it. |
| `pending_delivery.json` | One shared file recording every session's outstanding subscribe/unsubscribe intents, read and rewritten directly by `service/pendingdelivery.go`; it has no per-row identity that benefits from relational storage. |
| `delivery-locks/<session>.lock` | Pure OS-level mutual exclusion for a delivery decision (`service/deliverylock.go`), not data — a `flock`, not a fact worth persisting. |

The importer validates that none of these (or their own lock files) is
still held by a live writer, but never reads them for data or copies them:
see "One-time importer inventory" below.

`Session.Message` is not a stored field on any table: a session's
self-reported status line is derived from the most recent
`plect.status_message` event on its live row (an ordinary `events` row, no
special table), read via one indexed query — see "Status message" below.
`Session.Branch` is likewise not a stored field: a checked-out branch is git
vocabulary, and core stays version-control-agnostic by identity, so the fact
lives only in the `@workflow` pseudo-node's own `node_executions.outputs_json`
(a git-backed workspace provider's own setup output), read through
`domain.SessionBranch` by the handful of call sites that want it.

`population_members.session_name` is a recorded fact, not an enforced foreign
key: admission can record a member's intended session name before that
session's own row exists, and before an id could be known for it (unlike
`sessions.population_workflow`/`population_name` below, there is no later
point at which this column's value gets promoted to an id — see its own
comment in `schema.sql`). `Session.Population` (the session-side reference to
the population that owns this session) is a genuinely separate authority
from `population_members.session_name`, not a derivable join: the one write
path that sets it (`population/engine.go`'s admission, via
`population/runtime.go`'s `upPopulation`) creates the session through the
population's own hook *before* it records the member row, so a read between
those two steps would see a session with no population yet if the field were
derived by join instead of stored on the session itself.
`sessions.population_workflow`/`population_name` are that stored reference,
with a composite foreign key to `populations(workflow, name)` and `ON DELETE
SET NULL`. The foreign key is satisfiable at every write: `admit` only runs
from `Reconcile`, which only runs once `e.state.Population` already found a
row, and that row is upserted by `ApplyPoll`/`ApplyAppearance` before
`Reconcile` is ever called — so a population always exists before any session
references it. `population_members.session_name` remains the authority for
*current* membership (a tombstoned or reassigned member can disagree with a
session that has not yet been destroyed or updated); the promoted columns are
the authority for what a session was created under, which does not change for
that session's lifetime once admission succeeds.

`(workflow, name)` is a population's own domain identity — a workflow's
declared population, its config address plus population name — not the
`"workflow/name"` string built for JSON map keys and in-memory lookups
elsewhere in `internal/population`; the persistence boundary parses that
concatenation back into its two parts on every population read or write
rather than storing it as an identity.

`task_instances.named` replaces what would otherwise be a duplicated
instance-identity string: a `--name` a caller gave the instance is always
either empty or exactly `instance_name`, so `named` is the only bit that
does not already live in `instance_name` itself. `finalized_at` on both
`node_executions` and `task_instances` is nullable and orthogonal to
`status`: `plect task finalize` can record completion while status stays
`produced`, awaiting a later `plect task cleanup`, so it is a timestamp
fact rather than a state folded into `status`'s CHECK.

`up_reservations.parent_session_name` is nullable rather than carrying a
`"@virtual-root"` sentinel string: a reservation counted against the
virtual root's own `max_up_children` cap (a parentless session, or one
whose parent is the `root:` pseudo-parent) has no real parent to name, so
`parent_session_name` is `NULL` and `virtual_root` is `1` instead — a CHECK
enforces that exactly one of the two holds. The sentinel itself
(`domain.VirtualRootReservationParent`) still exists as a Go-level value at
the `state.Store`/`ReserveUpSlot` call-site boundary, translated to and
from these two columns only inside the persistence package.

`Session.Nodes` (static workflow-DAG nodes, including the `@workflow`
pseudo-node) and `Session.Tasks` (dynamic instances created at runtime via
`plect task setup`) map onto `node_instances`/`node_executions` and
`task_instances` respectively — one domain collection per identity table
(plus, for nodes, the per-attempt `node_executions` child it now owns; see
"Node execution identity" below), with no composition or
`Dynamic`-discriminated derivation at the persistence boundary.
`task_instances.id` is a ULID minted once when a dynamic instance's row is
first created and preserved by every later write that still names the same
`(session_id, instance_name)` — an ordinary `Put`/`Update` upserts the
row and keeps its existing id. Only a cleanup (the instance disappearing
from a write's `Tasks` map, so the row is deleted outright) followed by a
new setup under the same `instance_name` mints a fresh id, which is what
gives that recreated instance a fresh `task_done_when_states`/
`task_done_when_judges` history rather than resurfacing the retired
instance's. `TaskState.Layers` (a nested effect/task chain's per-layer
record, ordered outermost-first) splits into `node_execution_layers`/
`task_instance_layers` by the same static/dynamic distinction, each row's
`position` column preserving that order — there is no shared polymorphic
layer table, since the two parents key differently.

`task_done_when_judges` does not store the judged session/instance: the
owning `task_instances` row's own `(session_id, instance_name)` is always
the judged side (the one write path that records a verdict always stores it
on the same session/instance it names as the target), so
`DoneWhenJudge.TargetSession`/`Instance` are derived from that join at read
time rather than duplicated as columns. `relation`'s CHECK set is the seven
`domain.SessionRelation` values; unlike `judge_workflow`, a judge always has
a computed relation, so there is no eighth "unset" value to admit.

`Session.ChannelValidationHealth`/`ChannelDeliveryHealth` are two
independent open-failure-streak trackers of one shape: validation (checked
once, at a dispatcher's build) and delivery (checked per event) run on
entirely separate schedules, so a success of one kind must never clear the
other's still-open streak. `session_channel_health` holds at most one row
per `(session_id, kind)`; a kind with no open streak simply has no row.

The database does not contain plugin-owned configuration or plugin-private
state. In particular, plugin catalog and lock files remain configuration, and
the Slack adapter's subscriber file remains adapter-owned data. These files
are not part of the runtime import.

## Node execution identity

A workflow node and one of its setup attempts are different things.
`node_instances` is the logical node's identity within a session —
`(session_id, node_id)`, and nothing else. `node_executions` holds one row
per setup attempt, with its own ULID `id`, `sequence` (the same
monotonically increasing instantiation counter `TaskState.Seq` always was),
and every per-attempt fact the old single-row `node_instances` used to carry
directly: `task_id`, `name`, `scope`, `resource`, `status`, inputs, outputs,
state, resource observation, `done_when`, extra `done_when`, error, and
lifecycle timestamps. A node_id can outlive several execution generations
across a session's lifetime — a workflow revision remaps it onto a
different task, or a failed setup is retried after an operator releases the
old allocation — and each generation gets its own `node_executions` row
rather than overwriting the last one in place.

At most one row per `(session_id, node_id)` may be unreleased (`status <>
'cleaned'`) at a time (`node_executions_one_unreleased_idx`, a partial
unique index). `task.RunSetup` enforces this at the point a setup would
otherwise begin: if a node's existing, unreleased record names a different
declaration — a different `task_id`/`scope`, or a nesting chain whose layer
count or effect ids differ — the setup is refused with an actionable error
naming both the retained and requested declarations, rather than silently
discarding the old record's own release recipe. This is deliberately not
gated by a force flag: no caller needs one yet, and refusal (pointing the
operator at `plect down`/`plect destroy`) is a complete, correct answer to
"an unreleased allocation must not be silently replaced." A retry of the
*same* declaration (a failed setup re-run, or an already-produced node
whose liveness check failed and was invalidated) is not a new declaration,
so it updates the existing unreleased row in place, preserving its id.

Persistence enforces the identity side of this independent of the gate
above: `writeTasksTx`'s node reconciliation looks up the current unreleased
execution for `(session_id, node_id)` and updates it in place when one
exists, or inserts a fresh row when none does. A node absent from an
incoming write's `Nodes` map is pruned only when its latest execution has
already reached `cleaned` — an absent-but-unreleased node is left
untouched, so a workflow revision that simply stops declaring a node (or
any other caller that happens to omit it from one write) can never discard
its execution record or outstanding cleanup obligation merely by omission.

Looking up "the current unreleased execution" by node_id alone is not
sufficient to protect a specific generation once more than one writer can
touch the same session: `contracts/state.TaskState.ExecutionID` names the
row a loaded state actually came from, and `task.RunSetup` carries it
forward only when a fresh attempt continues that exact row (a
same-declaration retry) — a genuinely new attempt (first setup, or one
reviving a node this same call already released via a liveness-triggered
cleanup) leaves it empty. When `ExecutionID` is set, `upsertNodeExecutionTx`
resolves and updates exactly that row by id, regardless of its current
status, refusing the write instead if the row is gone or if it has since
been released (`status = 'cleaned'`) while the incoming write's own status
has not — the one status combination this does not refuse is both sides
already `'cleaned'`, which is a caller re-persisting a checkpoint it already
wrote, not a conflict. Resolving by id rather than "current unreleased"
is also what lets that re-persist target the same row instead of minting a
duplicate cleaned one, since the "current unreleased" query would no longer
see it. A write whose `ExecutionID` is empty makes no claim about continuing a
specific row, so it instead falls back to "the current unreleased row for
this node_id, if any" — update in place, or insert a fresh row when none
exists. Two identity gaps live in this fallback, tracked by
[#513](https://github.com/kecbigmt/plecture/issues/513) rather than
closed here: a same-pass liveness-invalidate-then-rebuild's
intermediate release is never itself persisted (`task.RunSetup` operates on
one in-memory state per node id and cannot represent "old row now cleaned"
and "new row just produced" at once), so it collapses onto an update of the
released row instead of minting a fresh generation; and a genuinely new
first attempt racing a different writer's concurrent setup of the same node
lands on the same fallback and cannot tell the two cases apart either. See
the ADR's Consequences section.

Release is the only thing that clears a node: `service.unifiedTeardownList`
enumerates every unreleased execution directly from `session.Nodes` — not
from the *current* plan, which has nothing to say about a node it no
longer declares — and `persistence.PruneReleasedNode`/`ResetNodes` are the
two explicit ways a row is actually removed (a caller, such as
`persistStaleWorkflowCleanup`, that already knows a specific node's cleanup
just succeeded; or `--force-recreate`'s own deliberate whole-runtime wipe,
which discards every node's history on purpose instead of retaining it).

### Retained cleanup contract

Release must be possible from a node's own retained record, not from
whatever the *current* task/effect definition says: a workflow revision
that changes or removes a definition must not change how an already-running
allocation gets cleaned up. `node_executions.cleanup_json` (a plain node)
and `node_execution_layers.cleanup_json` (a nested node, one entry per
layer) hold this: the resolved cleanup action (`lang.Action`, itself plain
data — no compiled or unexported internals), its source path and ownership,
and, per layer, the outward joint (`config.OutputBinding`) a layer's
cleanup needs to read its own public contract. This is exactly the
schema-free shape `effect.CleanupLayers` already built fresh from config for
teardown before this change; retention only moves *when* that shape is
computed, from teardown time to setup time. The compiled
`InputsSchema`/`LocalsSchema`/`OutputsSchema` a setup-time `effect.Layer`
also carries are never part of it — cleanup never read them in the first
place, so there is nothing there that resists JSON serialization.

`task.RunSetup`'s own internal `retainCleanup`/`effect.RetainLayerCleanup` produce this JSON at setup
time; `task.DecodeRetainedCleanup`/`effect.LayersFromRetained` decode it at
release time. `service.resolveNodeCleanup` prefers the retained contract
and falls back to re-resolving the current task definition by the
execution's own retained `task_id` — tolerant of a missing or changed
definition, exactly like a dynamic instance's teardown already was — only
for an execution that retained nothing: one from before this change (the
migration below leaves `cleanup_json`/`plugin_ref`/`execution_dir` `NULL`
for every carried-forward row, since a SQL migration cannot read
`config.toml`), or a nested node's own composed `TaskState.Cleanup`, which
is always nil (its per-layer contracts carry the recipe instead).
`effect.LayersFromRetained` reports its whole chain as unavailable if any
one layer lacks a retained contract, so a caller never mixes retained and
re-resolved layers in one release.

`node_executions.execution_dir` is the absolute working directory the
setup ran in (the session's workspace directory at setup time), and
`node_executions.plugin_ref` is the resolved plugin catalog address, path,
and content revision (`plugins.Mounted.Revision`, the plugin's own entry in
`plect.lock`, independent of the catalog's own mutable tree) a plugin-owned
cleanup action's `bin` references resolve against —
`"<catalog-alias>/<plugin-path>@<revision>"`, or just the address for a
non-reproducible (editable-path) mount `plect.lock` does not pin. Both are
retained per execution so release does not depend on the session's
*current* `workspace_dir` or the plugin catalog's current state.
`task.RunCleanup` consumes both: a plain node's cleanup runs in its
retained `execution_dir` rather than the session's current workspace, and
refuses to run at all when the currently mounted plugin's own revision no
longer matches the retained `plugin_ref`. Pinning a revision this way
identifies *which* plugin content a cleanup action ran against; it is not
a general artifact store, and does not itself guarantee that content is
still reachable if the catalog has since been garbage collected — that
guarantee needs a concrete consumer before it is worth building. The
revision check does not yet extend to a nested node's per-layer cleanup,
since `effect.RetainedLayerCleanup` does not yet retain a per-layer
`plugin_ref`.

`ExecutionDir`, `PluginRef`, `Cleanup`, and `ExecutionID` are excluded from
`contracts/state.TaskState`/`LayerState`'s ordinary JSON output
(`json:"-"`): they are persistence-internal retention details a node
execution's own release logic (or, for `ExecutionID`, its own write-conflict
check) needs, not facts an external consumer (the Web UI, an MCP tool
response, `plect status --json`) needs to see, and excluding them by
default avoids ever having to reason about which of their contents (a
cleanup script's literal text, an input value it closes over) would be
safe to disclose there.

### Release ordering

`node_execution_dependencies` snapshots, at a node's own setup time, which
other nodes' *currently unreleased* executions `task.Resolved.DependsOn`
named (itself derived from node input bindings, not authored directly) —
`execution_id` is the dependent, `depends_on_execution_id` the
prerequisite. A dependency with no currently unreleased execution (already
cleaned, or never set up) is simply not recorded; there is nothing left to
order release against. Because the edge is stamped once, at creation, it
survives a later config change that rewires or entirely drops the node
that originally expressed it — release ordering never re-derives edges
from the *current* plan.

`service.orderTeardownItems` uses these edges (Kahn's algorithm: a
dependent is released before its recorded prerequisites) instead of
ascending `Seq` alone, falling back to `Seq` to break ties and, for the
whole list, if a cycle is ever found (which retained, previously-valid
edges should never produce). Plain `Seq` order already gets this right in
the common case — a prerequisite's `Seq` is always lower, since it was
necessarily created first — but not once an old, still-unreleased
execution's own recorded dependency needs to be honored regardless of
what has been instantiated more recently elsewhere in the session.

Because a node write is topologically ordered within `writeTasksTx` itself
(`orderNodesByDependency`, prerequisites before dependents), a dependency
edge to another node in the *same* write is always resolvable: the
prerequisite's row already exists by the time the dependent looks up its
current execution id.

See `2026-09-07-node-execution-identity.md` for the decision this schema
implements; the migration from the prior single-row `node_instances` shape
(`app/internal/persistence/migrations/20260907085032_add_node_execution_identity.sql`)
carries its own reasoning for how it preserves existing rows.

## Session identity and lifecycle

Sessions are retained across destroy. `sessions.id` is a ULID minted once,
by `plect up`/`plect create`, when a session is first created (or recreated
under a reused name after a prior destroy); it never changes thereafter. A
session row *is* one incarnation: there is no separate per-incarnation
table (see "Event positions and cursors" below for how events and cursors
key off it directly).

`name` stays a mutable label but is unique only among live rows:
`CREATE UNIQUE INDEX sessions_live_name ON sessions(name) WHERE status <>
'destroyed'`. A destroyed session's name is therefore free for a later
create to reuse, minting a new row (new `id`) rather than reviving the old
one; reads by name always resolve the live row, and a superseded
incarnation stays reachable only by its own `id`.

`status TEXT CHECK (status IN ('down', 'up', 'destroyed'))` is a lifecycle
phase, not liveness or health — `domain.HealthState` (a derived runtime
observation, in the `health_*` columns) and `config.RunScopeUp` (a derived
fact about which run-scoped tasks are produced) answer those
separately. Transitions: create leaves a session `down`; `plect up` moves it
to `up`; `plect down` moves it back to `down`; `plect destroy` moves it to
`destroyed`, which is terminal and sets `destroyed_at` (`CHECK ((status =
'destroyed') = (destroyed_at IS NOT NULL))`). A launch that fails leaves
`down` (failure-atomic, per the runtime failure model). `plect down`/`plect
up`/`--force-recreate` all keep the same row and `id`; only a destroy
followed by a same-name create mints a new one.

Because no write path ever deletes a `sessions` row,
`parent_session_id`/`root_session_id` use `ON DELETE NO ACTION` rather than
`SET NULL`, and a destroyed parent keeps every child's history — including
its own parent link — intact: `child.ParentSession` still resolves to the
destroyed parent's name, even though that parent is absent from a
live listing (`plect ls`, `plect status`, and the Web UI all hide destroyed
rows by default). History reads (an event read, or a listing that opts into
`--all`) take an `id` rather than a name. Retention and purge of destroyed
rows and their events is a separate, later concern this design does not
address.

`parent_session_id`/`root_session_id` are resolved against the referenced
session's live row once — normally at the child's own creation, but a
session created with no parent yet may still adopt one on a later write,
since nothing has been resolved to fix in place — and never re-resolved
after that. A write to a session whose parent columns are already set
passes them through unchanged regardless of what its in-memory
`ParentSession` (a name) says, so a parent later destroyed and recreated
under the same name (a distinct `id`) never retargets an existing child
onto the new incarnation just because the child itself is written again.
`contracts/state.Session` carries the resolved `ParentSessionID`/
`RootSessionID` alongside `ParentSession`, which is a read-side projection
of them onto the referenced row's current name (with a `root:` prefix for
`RootSessionID`) — not itself the identity a write resolves against.

## Status message

`Session.Message` is not a stored field: the fact lives entirely in the
session's `plect.status_message` event stream. The current value is the
most recent such event on the session's live row; `metadata.cleared =
"true"` (or an empty `summary`) means no message is set, and the
event's own `time` is the message's `updated_at`. Readers (`plect ls`'s
MESSAGE column, `plect status`, the Web UI session list and detail) resolve
it through one query — `service.LatestStatusMessage`, backed by
`events_session_id_type_sequence_idx` (`(session_id, type, sequence)`) so
each lookup is an index seek, not a per-session table scan: `SELECT ...
FROM events WHERE session_id = ? AND type = 'plect.status_message' ORDER BY
sequence DESC LIMIT 1`. The writer (`service.SetMessage`) reads the same
latest event to decide whether the incoming text actually changed before
appending a new one; a no-op call appends nothing.

## Schema excerpts

`app/internal/persistence/schema.sql` declares the application-owned tables.
The following excerpt shows the session relationship and event ordering shape;
the remaining tables follow the keys in the inventory above.

```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('down', 'up', 'destroyed')),
    destroyed_at TEXT,
    parent_session_id TEXT REFERENCES sessions(id) ON DELETE NO ACTION,
    root_session_id TEXT REFERENCES sessions(id) ON DELETE NO ACTION,
    resource_id TEXT,
    alias TEXT,
    workflow TEXT NOT NULL,
    workspace_dir TEXT,
    population_workflow TEXT,
    population_name TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((status = 'destroyed') = (destroyed_at IS NOT NULL)),
    CHECK (NOT (parent_session_id IS NOT NULL AND root_session_id IS NOT NULL)),
    CHECK (root_session_id IS NULL OR root_session_id <> id),
    CHECK ((population_workflow IS NULL) = (population_name IS NULL)),
    FOREIGN KEY (population_workflow, population_name) REFERENCES populations(workflow, name) ON DELETE SET NULL
);

CREATE UNIQUE INDEX sessions_live_name ON sessions(name) WHERE status <> 'destroyed';
CREATE INDEX sessions_alias_idx ON sessions(alias);
CREATE INDEX sessions_parent_idx ON sessions(parent_session_id);

CREATE TABLE events (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    time TEXT NOT NULL,
    type TEXT NOT NULL,
    source TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound', 'internal')),
    summary TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL CHECK (json_valid(metadata_json))
);

CREATE UNIQUE INDEX events_session_id_sequence ON events(session_id, sequence);
CREATE INDEX events_session_id_id_idx ON events(session_id, id);
CREATE INDEX events_session_id_type_sequence_idx ON events(session_id, type, sequence);

CREATE TABLE event_cursors (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('delivery', 'tick', 'heartbeat')),
    next_sequence INTEGER NOT NULL CHECK (next_sequence >= 0),
    PRIMARY KEY (session_id, kind)
);
```

The migration history begins with a transaction-only goose migration. Goose
records the migration version in its ledger in the same transaction as the
schema changes; migration files do not use a `NO TRANSACTION` directive.
Migrations are Atlas-generated from `schema.sql` (see
`app/internal/persistence/doc.go`) wherever Atlas's own diff is usable
as-is; a structural change Atlas cannot safely diff against a database
already holding rows (see `app/internal/persistence/migrations/`'s own
`20260907002408_...` migration for the case this design's own session-id
rework hit, and its `20260907085032_...` migration for the node execution
identity split — see `2026-09-07-node-execution-identity.md`) is
hand-written instead, reaching the identical structural end state
`schema.sql` declares. Either way the migration file's exact SQL
text — quoting, per-index `CREATE` statements — is authoritative over any
excerpt here.

The append query obtains the next sequence inside the caller's write
transaction. The persistence append method resolves the session's live row
first — rejecting the write if none exists — and retries a transaction only
when SQLite reports a busy conflict; it never calculates a position outside
the transaction.

```sql
-- name: NextEventSequence :one
SELECT COALESCE(MAX(sequence), 0) + 1
FROM events
WHERE session_id = ?;

-- name: InsertEvent :exec
INSERT INTO events (
    id, session_id, sequence, time, type, source, direction,
    summary, body, metadata_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
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
| `Put` | Reconcile `Nodes` against `node_instances`/`node_executions`: a node absent from `Nodes` is pruned only once its latest execution already reached `cleaned` (see "Node execution identity"); a node present is upserted into its current unreleased execution, or a fresh one when none exists. Reconcile `Tasks` against `task_instances` unconditionally on absence instead (upserting each current dynamic instance, preserving its `id` across an ordinary update, then deleting any instance no longer present), replacing each surviving instance's completion rows; normalize the parent relation — all in one write transaction. | Preserves one durable checkpoint for the supplied session; it no longer rewrites unrelated sessions; an unreleased node execution's own record and cleanup obligation survive a write that merely stops mentioning it; and a dynamic instance's `id` and completion history survive an ordinary update instead of resetting on every write. |
| `Update` | Read the named session and its workflow-node/task-instance/completion rows, run the in-process callback, then write that session's changed rows in one write transaction. | Preserves read-modify-write atomicity and the missing-session error. The callback remains local and must not perform external work. |
| `UpdatePopulation` | Read or create one population and its members, run the callback, then replace that population's members in one write transaction. | Preserves an atomic population snapshot without serializing unrelated sessions. |
| `ReserveUpSlot` | Delete reservations whose recorded PID is no longer live, read the parent’s active children and reservations, enforce the cap, and insert the child reservation in one write transaction. | Preserves the live-holder rule and the rejection for an already-reserved child. |
| `ReleaseUpSlot` | Delete the named reservation in one write transaction. | Remains idempotent and best-effort at its existing call sites. |
| `Destroy` | Update the session's live row to `status = 'destroyed'`, `destroyed_at = now` (the same upsert path `Put` uses, since the row's `id` and every other column are untouched), then delete its up-slot reservation, in one write transaction. | Retires the session (hidden from a live listing, its name freed for reuse) without deleting it: its rows, tasks, and event history remain reachable by `id`. |
| `eventlog.Store.Append` | Resolves the target's live session row, minting a minimal placeholder one first if none exists yet (a plain event target, not necessarily one created through Create), then in one write transaction reads `NextEventSequence` from `MAX(sequence)` against that row and inserts the event. | Preserves the "publish needs no prior create" contract while keeping each append's own sequence assignment atomic. |
| `CommitCursor` | Upsert one consumer’s next sequence in one write transaction. | Preserves at-least-once dispatch and reactor restart behavior. |

`WriteTombstone` is not in this table: it stays a direct file write (see
"Stays file-based after cutover" above), not a database transaction.

A session's status line is not part of this table: it is derived entirely
from its `plect.status_message` event stream (see "Status message" above),
so `service.SetMessage` is a single append, not a session write plus an
append — a no-op call (the incoming text matches the latest event) appends
nothing at all. Lifecycle event recording, judge-recorded events, task
instruction events, terminal events, and population notifications remain
independent best-effort appends where their existing callers ignore append
failures. This design does not introduce event sourcing or claim that every
state transition produces an event.

Destroy preserves its existing checkpoint order until its service contract is
changed: external cleanup, session checkpoint, tombstone write, lifecycle
append, then the session's `Destroy` status transition
(`app/internal/service/lifecycle_destroy.go`). The SQLite implementation keeps
each database callback atomic but does not collapse those independently
observable milestones into a new all-or-nothing lifecycle transaction.

## Migration access gate

SQLite's single-writer rule is insufficient: it does not make a running binary
aware that its schema assumptions are incompatible. `persistence` therefore
uses two local advisory lock files next to `storage.db`:

- `storage.db.access.lock` is the data-access gate. Every query and explicit
  transaction takes a shared lock for its duration. A migration takes its
  exclusive lock for the full migration.
- `storage.db.coordination.lock` serializes migration intent. A normal access
  briefly takes a shared lock before taking the access gate. A migration takes
  it exclusively, writes and fsyncs `storage.db.migration.json`, and retains it
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

A development build — one `app/internal/version.IsDevelopmentBuild` reports
true for, because the release pipeline never stamped it — additionally
refuses to advance a database it did not create: a ledger already holding an
applied migration (schema version greater than zero) behind the embedded
migration set. The refusal names both schema versions and points at
`XDG_DATA_HOME` (isolate onto a scratch database) and
`plect storage migrate --allow-dev-build` (migrate this one deliberately) as
the two ways to proceed, and makes no change, the same as the newer-than-
supported refusal above. A database at schema zero carries none of that risk
— this call is the one creating it — so a development build migrates it to
the embedded target without needing the flag. A release-stamped build keeps
today's automatic migration in every case, flag or not.

Any packager building plect from source, not only the release pipeline's own
matrix build, must inject the version the same way: a source build that
skips `-ldflags "-X github.com/kecbigmt/plecture/app/internal/version.Current=<version>"`
leaves `Current` at its unstamped placeholder and is a development build by
this section's definition, regardless of what version string the packaging
system otherwise derives (a Nix flake revision, a distribution's own package
version, ...) for purposes outside this binary. Such a build refuses implicit
migration of an existing store exactly as above until the packager passes
that flag.

The resident service keeps pools but uses the persistence access gate around
each database operation, not around the process lifetime. Its HTTP event
routes answer an unavailable/retryable response while a migration blocks an
operation. `plect-web` performs the same startup check and its handlers use
the same facade. Plugin executables never open core's database; a plugin that
invokes `plect` or the local event service receives the same wait-or-refuse
result. This keeps provider code outside migration coordination and prevents a
second storage authority.

## Event positions and cursors

An event cursor is the opaque `event.Cursor{Off, Ord, StreamID}` value, with
`CursorVersion` set to `2`. `Off` is the exclusive logical sequence for the
selected session incarnation: `1` starts at the first row, and a cursor
after event sequence `n` has `Off == n + 1`. `StreamID` is that
incarnation's `sessions.id`, opaque to every caller that carries it, and
`Ord` is the requested order.

A session row is the log of one incarnation, not of a session name (see
"Session identity and lifecycle" above): a session create mints a new row
(a fresh `id`); a down/up or `--force-recreate` keeps the existing row and
`id`, so its events and `events.sequence` continue unchanged. Destroying a
session never deletes its row, so a destroyed incarnation's `events` rows
survive, reachable by their own `id`, but a later create under the same
name is a new session with a new `id`. A read by session name (`plect event
list`, the Web API history/SSE, dispatcher/reactor cursors, the subtree
read) resolves to the live row for that name. A `StreamID` mismatch is
therefore the expected outcome of a destroy and same-name recreate: a v2
cursor issued for the destroyed incarnation fails validation against the
new one, and the client's recovery path (discard the cursor, refetch
history) is exactly the guard's purpose. Reading the destroyed incarnation's
own events still requires its own `id`, not its session name.

`EventPage` decodes only version-2 cursors, validates `Ord` and `StreamID`
against the selected stream, and reads `sequence >= Off` in ascending order.
Its next cursor has the exclusive position after the final scanned event. It
returns an empty page for a missing stream, preserves the current ascending
and non-paginating descending contracts, and rejects a stream-id or order
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
trailing partial line rejects the import. Dispatcher and reactor positions
import into `event_cursors` under their own `kind` (`delivery`, `tick`); the
heartbeat position imports under `heartbeat`.

## One-time importer inventory

The one-time importer targets every `sessions`/`node_instances`/
`node_executions`/`task_instances` destination column below as this
design's own named-column schema (no `record_json`), with a legacy
incarnation reference resolving to a minted `sessions.id`. Each legacy
node produces exactly one `node_executions` row (its first recorded
execution), the same rule the node-execution-identity migration itself
applies when it carries an already-cut-over database's own pre-change
rows forward — see "Node execution identity" below. This table records
the source-side validation contract; the exact per-column legacy-JSON-field
mapping is a separate concern from that contract.

The import command runs only against an operator-created backup while writers
are stopped. It builds and validates a temporary database, validates it again,
then atomically promotes it as `storage.db`. It never writes JSON and JSONL
alongside the database. The command reads the following runtime paths.

| Legacy path | Validation and destination |
| --- | --- |
| `state.json` | Parse once; require the supported legacy envelope version; validate layer effect identities and tree relationships; import sessions, tasks, completion state, populations, and reservations. Each session's legacy `TickBackoff.LastLogPosition` translates through the same byte-boundary index as a `.cursor.<consumer>` file and imports into `event_cursors` as that session's `heartbeat` position. |
| `state.json.lock` | Confirm it is not held before import; do not copy it. The access gate replaces it. |
| `events/<escaped-session>/log.jsonl` | Decode complete lines in byte order; reject invalid event identity, session mismatch, duplicate ID, and a malformed complete line; discard only a trailing partial line, matching the live reader; import stream and events. A record with no `direction` imports as `internal`, counted in the import summary. |
| `events/<escaped-session>/.gen` | Read one trimmed non-empty stream identifier when present and reuse it as the imported session row's `id`; otherwise mint a fresh id after recording that no old page cursor survives cutover. |
| `events/<escaped-session>/.cursor.<consumer>` | Parse a non-negative decimal boundary, validate it against the log boundary index, map `<consumer>` to its `event_cursors.kind` (`dispatcher` imports as `delivery`, `tick-reactor` imports as `tick`), and import the translated position. |
| `events/<escaped-session>/tombstone.json` | Stays file-based (see "Stays file-based after cutover" above); the importer never reads or copies it. |
| `events/<escaped-session>/chain_attempts.json` | Stays file-based; never read or copied. |
| `events/<escaped-session>/.lock` | Confirm it is not held before import; do not copy it. The database transaction and access gate replace it. |
| `pending_delivery.json` | Stays file-based; never read or copied. |
| `pending_delivery.json.lock` | Confirm it is not held before import; do not copy it. |
| `delivery-locks/<escaped-session>.lock` | Confirm no lock is held; do not copy it. The delivery decision keeps its service-level serialization through a database-backed lock/transaction. |

The importer inserts every `populations` row before any `sessions` row that
references it: `sessions.population_workflow`/`population_name` is a
composite foreign key to `populations(workflow, name)`, so a session whose
legacy `Population` field names a population the legacy `populations` map
never recorded (state predating that field, or a hand-edited file) fails
import rather than promoting a database with an unsatisfiable reference.

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
