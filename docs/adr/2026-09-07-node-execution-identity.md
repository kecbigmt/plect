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

Three security-relevant obligations apply to whatever this record becomes:
retained cleanup assets and their integrity/modification story; sensitive
setup inputs/environment must not leak through inspection, Web, MCP, or
audit surfaces by default; and a destructive cleanup's target-identity
check is an adapter's job, not something core invents a generic framework
for.

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
state. An execution that retained nothing — one from before this change, or
a nested node's own composed `TaskState.Cleanup`, which is always nil —
falls back to re-resolving the current definition by the execution's own
retained `task_id`, tolerant of drift exactly like a dynamic instance's
teardown already was.

`service.unifiedTeardownList` (used by `plect down`/`plect destroy`, and
internally by `--force-recreate`) now enumerates every unreleased execution
directly from `session.Nodes`, not from the current plan, so a node the
workflow no longer declares is still released — using what that execution
itself retained, never whatever the current plan says its node_id
currently means. Release order follows each execution's own recorded
`DependsOn` edges (Kahn's algorithm, dependents before prerequisites,
falling back to ascending `Seq` to break ties and, for the whole list, on
an unexpected cycle) instead of ascending `Seq` alone.

### Security obligations disposition

- **Retained cleanup assets.** The retained contract is a JSON snapshot of
  already-declared config values (`lang.Action`, `config.OutputBinding`),
  stored in the same database rows as every other execution fact, under
  the same write authority (the process holding the SQLite write lock) —
  no separate artifact store, and no new write path outside the existing
  `writeTasksTx`. `plugin_ref`'s content revision identifies *which*
  plugin content a cleanup action ran against; it does not itself
  guarantee that content remains reachable if the catalog has since been
  garbage collected, and a managed retention store for that guarantee is
  explicitly deferred — no consumer needs it yet, and Nix-style GC-root
  retention is a real but separate design, not a byproduct of this schema
  change.
- **Sensitive setup inputs/environment.** `ExecutionDir`, `PluginRef`, and
  `Cleanup` are excluded from `contracts/state.TaskState`/`LayerState`'s
  ordinary JSON output (`json:"-"`), so they never reach the Web UI, an MCP
  tool response, or `plect status --json` by default. This does not extend
  to `inputs_json`/`outputs_json`, which already round-trip through those
  surfaces today for every existing table; a general secret-marking
  mechanism for declaration-owned inputs is a separate concern with no
  concrete consumer in this change, deferred rather than built
  speculatively.
- **Cleanup target identity.** Core retains the same resource-identifying
  facts it always did (`resource`, `outputs`) on the execution record; it
  does not gain, and does not attempt, a generic ownership-verification
  framework. Which check an adapter must run before a destructive cleanup
  (comparing a live resource's own identity against what setup recorded)
  remains that adapter's own contract, entirely outside `plugins/*` code
  this change touches.

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
  per-plugin entry), not a general artifact store.

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
