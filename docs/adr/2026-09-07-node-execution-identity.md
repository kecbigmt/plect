# Settle node execution identity and ownership before the host cutover

## Context

`node_instances`' primary key was `(session_id, node_id)`: one stored row
per logical workflow node, holding that node's status, inputs, outputs, and
every other per-attempt fact directly. A later setup for the same node_id —
a workflow revision remapping it onto a different task, or an operator
retrying after a failure — simply overwrote that one row. `writeTasksTx`
reinforced this: every `PutSession`/`UpdateSession` call replaced the
session's entire set of `node_instances` rows from whatever the caller's
in-memory `Session.Nodes` map currently held, so a node silently dropped
from that map (a workflow revision that stopped declaring it, most
concretely) lost its row outright, with no distinction between "already
released" and "still holds an allocation nobody has torn down yet."

This is a real design gap, not a hypothetical one: the target model this
schema serves (SQLite durable storage, `docs/design/sqlite-persistence.md`)
retains setup-attempt records — including partial or failed setups and
allocations whose cleanup is unfinished — and releases existing allocations
in their recorded dependency order, independent of whatever the current
workflow declaration says. A logical node id is insufficient once a node
can carry several execution generations across a session's lifetime, and
release ordering by declared dependency requires the dependency edge
itself to survive a config change that rewires or drops the node that
expressed it. The SQLite transition (`2026-09-06-sqlite-durable-storage.md`)
can still accommodate this restructuring cheaply; the live host cutover
this schema will serve cannot, once real allocations exist under the old
model.

The issue that opened this work also raised adjacent security-relevant
questions (retained cleanup asset integrity, sensitive-data disclosure,
adapter-side cleanup target identity). The owner later ruled those out of
this decision's acceptance, tracked instead by
[#513](https://github.com/kecbigmt/plecture/issues/513): this decision is
the execution-identity data model alone.

## Decision

Split node identity from node execution. `node_instances` becomes the
logical node's identity alone — `(session_id, node_id)`, nothing else.
`node_executions` holds one row per setup attempt, with its own ULID `id`
and every per-attempt fact the old row held directly. At most one row per
`(session_id, node_id)` may be unreleased (`status <> 'cleaned'`) at a
time, enforced by a partial unique index rather than by the application
alone. `node_execution_layers` replaces `node_instance_layers`, keyed by
execution id instead of node id, so a revised nesting chain never replaces
an older, unreleased chain's own release recipe. `node_execution_dependencies`
snapshots, at a node's own setup time, which other nodes' currently
unreleased executions its resolved `DependsOn` named — the recorded edge,
not a re-derivation from the current plan, is what release ordering
consults.

Persistence retains an unreleased execution when a write's `Nodes` map
merely stops mentioning its node_id; it prunes a node only once its latest
execution has already reached `cleaned`. Two things actually remove a row:
a caller that already knows a specific node's cleanup just succeeded
(`persistence.PruneReleasedNode`, used by `persistStaleWorkflowCleanup` so
a node this operation itself just finished releasing disappears from this
same result rather than waiting for a later write), and an explicit
whole-runtime reset (`persistence.ResetNodes`, used only by
`--force-recreate`'s own deliberate wipe, which discards every node's
history on purpose rather than retaining it — an ordinary write must
never do this implicitly).

`task.RunSetup` refuses, rather than silently overwrites, a setup for a
node whose existing unreleased record names a different declaration — a
different task id/scope, or a nesting chain whose layer count or effect
ids differ. This is not gated by a force flag: no caller needs one yet,
and refusal (pointing the operator at `plect down`/`plect destroy`) fully
answers "an unreleased allocation must not be silently replaced." A retry
of the *same* declaration updates the existing row in place, preserving
its identity, which is what makes a failed setup's own retry, and an
already-produced node's liveness-triggered rebuild, keep working exactly
as before this change.

Release resolves each execution's own retained cleanup contract instead of
re-reading the current task/effect definition: `node_executions.cleanup_json`
for a plain node, `node_execution_layers.cleanup_json` per layer for a
nested one. Both are exactly the schema-free shape `effect.CleanupLayers`
already built fresh from config for teardown before this change (the
resolved `lang.Action`, source path, ownership, and outward joint) —
retention only moves *when* that shape is computed, from teardown time to
setup time, and never touches the compiled `*jsonschema.Schema` fields a
setup-time `effect.Layer` also carries, since cleanup never read those in
the first place. `node_executions.execution_dir` and `.plugin_ref` (a
resolved plugin's catalog address, path, and content revision from its own
`plect.lock` entry) round out what a release needs without depending on
the session's current `workspace_dir` or the plugin catalog's current
state — `task.RunCleanup` runs a plain node's cleanup in its retained
`execution_dir` (falling back to the session's current workspace only for
an execution that retained none, i.e. a pre-this-change row) and refuses to
run it at all when the currently mounted plugin's content revision no
longer matches the retained `plugin_ref`, rather than silently cleaning up
against drifted content. An execution that retained nothing — one from
before this change, or a nested node's own composed `TaskState.Cleanup`,
which is always nil — falls back to re-resolving the current definition by
the execution's own retained `task_id`, tolerant of drift exactly like a
dynamic instance's teardown already was.

`contracts/state.TaskState.ExecutionID` names the specific `node_executions`
row a loaded state was read from. `task.RunSetup` carries it forward only
when a fresh attempt continues that same unreleased row (a same-declaration
retry); a genuinely new attempt — first setup, or one that revives a node
already released by a liveness-triggered cleanup within the same call —
leaves it empty. When set, `persistence.upsertNodeExecutionTx` resolves and
updates exactly that row by id (regardless of its current status, so a
caller re-persisting an already-cleaned checkpoint updates the same row
instead of mining a duplicate), refusing the write if the row is gone or
has since been released while the write's own status has not: without
this, two writers racing a release-then-recreate of the same node could
have the stale one overwrite the new generation's own release recipe with
data read before the race, defeating the very isolation per-execution
identity exists to provide. A write with no `ExecutionID` falls back to
"the current unreleased row for this node, if any" — see Consequences for
the one case (a same-pass liveness-invalidate-then-rebuild) this fallback
does not fully resolve.

`service.unifiedTeardownList` (used by `plect down`/`plect destroy`, and
internally by `--force-recreate`) now enumerates every unreleased execution
directly from `session.Nodes`, not from the current plan, so a node the
workflow no longer declares is still released — using what that execution
itself retained, never whatever the current plan says its node_id
currently means. Release order follows each execution's own recorded
`DependsOn` edges (Kahn's algorithm, dependents before prerequisites,
falling back to ascending `Seq` to break ties and, for the whole list, on
an unexpected cycle) instead of ascending `Seq` alone.

### Substrate this decision leaves for #513

This decision's retained facts and checks are the substrate
[#513](https://github.com/kecbigmt/plecture/issues/513) builds its own
acceptance on top of, not acceptance criteria of this decision itself:

- `cleanup_json`/`execution_dir`/`plugin_ref` are a JSON snapshot of
  already-declared config values, stored in the same database rows as
  every other execution fact, under the same write authority (the process
  holding the SQLite write lock) — no separate artifact store, and no new
  write path outside the existing `writeTasksTx`.
- `plugin_ref`'s content revision identifies *which* plugin content a
  cleanup action ran against, and `task.RunCleanup` checks it: a plain
  node's cleanup refuses to run when the currently mounted plugin's
  revision has drifted from what setup recorded. The same check is not
  wired for a nested node's per-layer cleanup, since
  `effect.RetainedLayerCleanup` does not retain a per-layer `plugin_ref`.
- `ExecutionDir`, `PluginRef`, and `Cleanup` are excluded from
  `contracts/state.TaskState`/`LayerState`'s ordinary JSON output
  (`json:"-"`), so they do not reach the Web UI, an MCP tool response, or
  `plect status --json` by default. `inputs_json`/`outputs_json` are not
  excluded, and already round-trip through those surfaces for every
  existing table.
- The execution record carries the same resource-identifying facts
  (`resource`, `outputs`) every prior shape did; it carries no
  ownership-verification framework, generic or otherwise.

## Consequences

- A node's execution history is retained across ordinary writes; only an
  explicit release (or `--force-recreate`'s deliberate wipe) removes a row.
  Long-lived sessions that cycle a node through many setup/cleanup
  generations accumulate `cleaned` history with no automatic pruning yet —
  "not unlimited history" is a real constraint this change does not fully
  resolve, and a retention/GC policy for fully-released executions is
  deferred pending a concrete need, consistent with the schema's own
  no-speculative-columns discipline.
- The setup-time refusal gate changes observable behavior for exactly one
  case: a workflow revision that remaps an already-produced, unreleased
  node onto a different declaration (task id/scope, or nesting chain
  shape) now fails `plect up` with an actionable error instead of silently
  rebuilding under the new declaration. Every other retry path (same
  declaration, liveness-triggered rebuild) is unchanged.
- `plect down`/`plect destroy` now discover and release a node the current
  workflow no longer declares, which `plect up`'s own separate
  stale-cleanup pass already did in the common case; this closes the gap
  for the two other lifecycle commands rather than changing `up`'s own
  behavior.
- Full nested-node cleanup retention exists (per layer); a nested node's
  own *setup*-time schemas are never retained, since cleanup never needed
  them. Plugin revision pinning is content-hash-based (`plect.lock`'s own
  per-plugin entry), not a general artifact store; the same revision check
  cleanup now performs is not yet extended to a nested node's per-layer
  cleanup, since layers don't retain a per-layer `plugin_ref` at all.
- Two identity gaps remain, tracked by [#513](https://github.com/kecbigmt/plecture/issues/513), not by this change:
  - A same-pass liveness-invalidate-then-rebuild (an already-`produced`
    node whose liveness probe fails, is cleaned up, and is immediately
    re-set-up within the same `RunSetup` call) collapses onto the released
    row's own id instead of minting a fresh one: `task.RunSetup` holds one
    in-memory `TaskState` per node id and cannot represent "old row now
    cleaned" and "new row just produced" at the same time, so the
    intermediate release is never itself persisted before the rebuild's
    own write. Closing this needs either a persistence dependency inside
    `RunSetup` to checkpoint the release before rebuilding, or a
    data-model change letting one write carry more than one execution per
    node id.
  - A fresh setup attempt (`ExecutionID` empty, since it has no row to
    name) that finds an unreleased row already present at write time
    cannot distinguish "a different writer created this concurrently"
    from the same-pass case above, so it updates that row in place rather
    than refusing. The `ExecutionID` guard protects the case it exists
    for — a writer that names a specific row it read — not this
    no-prior-identity one.

## Alternatives considered

### Keep one row per node, distinguish attempts with a version column

A `generation INTEGER` column on the existing `node_instances` row, bumped
in place on each new attempt, would preserve the current status but not
the current outputs/cleanup contract of a superseded, still-unreleased
attempt — exactly the data an interrupted release needs. A genuinely
separate row per attempt is what makes "retain the old one until released"
possible at all.

### Force-flag-gated reconstruction instead of unconditional refusal

Allowing `--force` to release an old unreleased execution and immediately
proceed with a new declaration under the same node id was considered, since
the security-review comments this schema responds to raise exactly this
question ("define when reconstruction is permitted after failed cleanup").
No caller needs this yet: `plect down`/`plect destroy` already exist as the
explicit, two-step release path, and a single combined operation is easy
to add later once an actual workflow needs it, without a schema change.

### Eagerly reconstruct retained facts from current config at migration time

Populating `cleanup_json`/`plugin_ref`/`execution_dir` for pre-existing
rows by reading `config.toml` during the migration was considered, since a
reviewer asked for exactly this. A goose migration is a pure SQL
transaction with no config access, by design (`2026-09-06-sqlite-durable-storage.md`'s
own migration-tooling decision keeps migrations reviewable and
deterministic); adding config-reading logic to a migration would break
that. The chosen fallback — resolve by the execution's own retained
`task_id` against the *current* definition, tolerant of drift, exactly
like a dynamic instance's teardown already did before this change —
reaches the same practical outcome for a pre-migration row without new
migration-time infrastructure, and degrades no worse than today's existing
behavior for that one case.

### Retain a nested node's full setup-time schemas, not just its cleanup contract

Serializing `effect.Layer`'s compiled `InputsSchema`/`LocalsSchema`/
`OutputsSchema` (not just its cleanup-relevant fields) was considered, so a
release could in principle re-validate before running. Cleanup has never
validated against these schemas — `effect.CleanupLayers` never compiled
them even before this change — so retaining them would add real
serialization work (a compiled `*jsonschema.Schema` has no JSON form) for a
capability nothing currently uses.
