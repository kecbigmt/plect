-- schema.sql is the hand-edited declarative authority for the database
-- structure described in docs/design/sqlite-persistence.md. Generate
-- migrations from it with Atlas Community Edition; do not hand-write
-- migration SQL against a structural change already captured here.
--
-- Runtime and event tables; session_tombstones/pending_deliveries stay file-based.
--
-- Nullability convention throughout: a column is NULL exactly when the
-- domain value can be genuinely absent (never observed/resolved yet, or an
-- optional fact); a column that domain logic always populates is NOT NULL
-- with no DEFAULT, so every write site states its value explicitly rather
-- than silently inheriting one from the database.
--
-- Timestamp encoding: every *_at / *_json timestamp column is a UTC
-- RFC3339 string with exactly nine fractional digits (e.g.
-- "2026-09-06T08:50:42.821423717Z"), or NULL when unset — never a
-- variable-width fractional part, so lexical order equals time order. See
-- timeconv.go.
--
-- _json column rule (Schema amendment 15, kecbigmt/plecture#434): a text
-- column holding JSON carries the _json suffix, and every _json column is
-- declaration-owned — its shape comes from a configuration-language or
-- provider schema, never from core structure. A _json column that can be
-- absent is nullable with a `CHECK (col IS NULL OR json_valid(col))`; the
-- two exceptions to well-formedness-only enforcement are events.metadata_json
-- (NOT NULL, provider-owned but always present) and node_instances/
-- task_instances.done_when_json (see that column's own comment for why a
-- core-owned shape stays embedded there instead of getting relational
-- columns).
--
-- population_workflow/population_name are the session-side half of
-- population membership: every write site that sets them (population's own
-- admission hook, population/runtime.go's upPopulation) runs after that
-- population's own row already exists (ApplyPoll/ApplyAppearance upsert
-- `populations` before Reconcile ever calls admit), so the FK is always
-- satisfiable at write time. population_members.session_name is recorded
-- separately and is the authority for *current* membership (a tombstoned or
-- reassigned member can disagree with a session that has not yet been
-- destroyed or updated); these two columns are the authority for what a
-- session itself was created under, which never changes for that session's
-- lifetime once admission succeeds.
--
-- Session identity and lifecycle: a session row IS one incarnation (the
-- per-incarnation event_streams row from PR #463 folds in here). `id` is
-- the durable identity, minted once and never reused; `name` is a mutable
-- label that is unique only among live (non-destroyed) rows, so a destroyed
-- session's name is free for a later create to reuse under a new `id`.
-- `status` records lifecycle phase (down/up/destroyed), not liveness or
-- health — health is a derived runtime observation in the health_* columns
-- below. Sessions are retained across destroy: no write path ever deletes a
-- sessions row, so parent_session_id/root_session_id use ON DELETE NO
-- ACTION rather than SET NULL, and a destroyed parent keeps every child's
-- history (including its parent link) intact. Retention/purge of destroyed
-- rows is a separate, later concern.
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('down', 'up', 'destroyed')),
    destroyed_at TEXT,
    parent_session_id TEXT REFERENCES sessions(id) ON DELETE NO ACTION,
    root_session_id TEXT REFERENCES sessions(id) ON DELETE NO ACTION,
    resource_id TEXT,
    alias TEXT,
    -- branch is a workspace provider's own setup output, promoted to a
    -- column (rather than left in provider-owned outputs JSON) because core
    -- reads it directly at many call sites — resource observation, template
    -- rendering, channel delivery wiring — the same way workspace_dir
    -- already is, not because it is core-owned structure.
    branch TEXT,
    workflow TEXT NOT NULL,
    workspace_dir TEXT,
    population_workflow TEXT,
    population_name TEXT,
    inputs_json TEXT CHECK (inputs_json IS NULL OR json_valid(inputs_json)),
    -- Health columns are all nullable and stay NULL until the first
    -- healthcheck sweep evaluates this session; health_last_state's CHECK
    -- set is domain.HealthState's four values. Health is a derived runtime
    -- observation, never a lifecycle status, so it is deliberately separate
    -- from the status column above.
    health_last_checked_at TEXT,
    health_last_activity_at TEXT,
    health_last_fingerprint TEXT,
    health_last_state TEXT CHECK (health_last_state IS NULL OR health_last_state IN ('healthy', 'unhealthy', 'stalled', 'undeclared')),
    health_last_reason TEXT,
    health_last_notified_at TEXT,
    health_notify_count INTEGER,
    -- Tick backoff's own log-position watermark is not a column here: it
    -- moved to event_cursors' `heartbeat` kind (see that table), since it is
    -- exactly the same shape as the delivery/tick cursors already there —
    -- a per-session read position for one consumer.
    tick_consecutive_unchanged INTEGER,
    tick_last_fingerprint TEXT,
    last_tick_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((status = 'destroyed') = (destroyed_at IS NOT NULL)),
    CHECK (NOT (parent_session_id IS NOT NULL AND root_session_id IS NOT NULL)),
    CHECK (root_session_id IS NULL OR root_session_id <> id),
    CHECK ((population_workflow IS NULL) = (population_name IS NULL)),
    FOREIGN KEY (population_workflow, population_name) REFERENCES populations(workflow, name) ON DELETE SET NULL
);

-- name is unique only among live rows: a destroyed session's name is free
-- for a later create to reuse, minting a new row with a new id rather than
-- reviving this one.
CREATE UNIQUE INDEX sessions_live_name ON sessions(name) WHERE status <> 'destroyed';
CREATE INDEX sessions_alias_idx ON sessions(alias);
CREATE INDEX sessions_parent_idx ON sessions(parent_session_id);

-- Session.Tasks splits into two tables by Dynamic: a static workflow-DAG
-- node (including the @workflow pseudo-node), keyed by its stable node id,
-- versus a dynamic task-document instance created at runtime via
-- `plect task setup`. The persistence layer reads both and composes the
-- one Tasks map the domain type and every core call site still see;
-- Dynamic itself is derived from which table a record came from and is not
-- a stored column on either.
-- finalized_at is nullable and orthogonal to status: `plect task finalize`
-- can record completion while status stays 'produced', awaiting a later
-- `plect task cleanup` — it is a timestamp fact, not a lifecycle state, so
-- it does not fold into status or its CHECK.
--
-- resource_observation_json/resource_observed_at split ResourceObservation:
-- the former is the observer's own state map (declaration-owned JSON), the
-- latter is core's own "when was this taken" fact, so it gets a real
-- timestamp column rather than living inside the opaque blob.
--
-- done_when_json stays a single opaque column rather than the relational
-- task_done_when_states/task_done_when_judges split that task_instances
-- gets: a static node's done_when is rare (no shipped workflow node relies
-- on it) and never relationally queried, so splitting it into per-judge
-- rows would add relational structure with no query that uses it. This is
-- a narrow, explicit exception to "core-owned structure is never stored as
-- JSON" for that reason alone — not a reopening of the record_json
-- grab-bag this migration removes.
CREATE TABLE node_instances (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL,
    task_id TEXT,
    name TEXT,
    scope TEXT NOT NULL CHECK (scope IN ('session', 'run')),
    status TEXT NOT NULL CHECK (status IN ('produced', 'failed', 'cleaned')),
    sequence INTEGER NOT NULL,
    resource TEXT,
    inputs_json TEXT CHECK (inputs_json IS NULL OR json_valid(inputs_json)),
    outputs_json TEXT CHECK (outputs_json IS NULL OR json_valid(outputs_json)),
    state_json TEXT CHECK (state_json IS NULL OR json_valid(state_json)),
    resource_observation_json TEXT CHECK (resource_observation_json IS NULL OR json_valid(resource_observation_json)),
    resource_observed_at TEXT,
    done_when_json TEXT CHECK (done_when_json IS NULL OR json_valid(done_when_json)),
    extra_done_when_json TEXT CHECK (extra_done_when_json IS NULL OR json_valid(extra_done_when_json)),
    error TEXT,
    setup_at TEXT,
    failed_at TEXT,
    cleaned_at TEXT,
    finalized_at TEXT,
    PRIMARY KEY (session_id, node_id)
);

-- One row per layer of a node instance's nested effect/task chain, ordered
-- by position (outermost-first, matching contract.LayerState's own
-- recorded order). No shared polymorphic layer table: node and task
-- instances have different parent keys, and a shared table would need a
-- nullable discriminated FK pair instead of one real foreign key.
CREATE TABLE node_instance_layers (
    session_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    effect_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('produced', 'failed', 'cleaned')),
    inputs_json TEXT CHECK (inputs_json IS NULL OR json_valid(inputs_json)),
    locals_json TEXT CHECK (locals_json IS NULL OR json_valid(locals_json)),
    outputs_json TEXT CHECK (outputs_json IS NULL OR json_valid(outputs_json)),
    env_json TEXT CHECK (env_json IS NULL OR json_valid(env_json)),
    heartbeat_ticks INTEGER,
    heartbeat_escalations INTEGER,
    setup_at TEXT,
    failed_at TEXT,
    cleaned_at TEXT,
    error TEXT,
    PRIMARY KEY (session_id, node_id, position),
    FOREIGN KEY (session_id, node_id) REFERENCES node_instances(session_id, node_id) ON DELETE CASCADE
);

-- A dynamic instance's id is a ULID minted once when the row is first
-- created (task setup) and preserved by every later Put/Update that still
-- names the same (session_id, instance_name) — only a cleanup (the row
-- disappearing from a write's Tasks map) followed by a new setup mints a
-- fresh one. done_when counters/fingerprints and judge verdicts are split
-- into their own tables below (keyed by this id), so a judge verdict is
-- never duplicated between two authorities.
--
-- named replaces a would-be duplicated instance-identity string: Name (the
-- `--name` a caller gave the instance) is always either empty or exactly
-- instance_name, so which case applies is the only bit that does not
-- already live in instance_name itself.
CREATE TABLE task_instances (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    instance_name TEXT NOT NULL,
    task_id TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('session', 'run')),
    status TEXT NOT NULL CHECK (status IN ('produced', 'failed', 'cleaned')),
    sequence INTEGER NOT NULL,
    resource TEXT,
    named boolean NOT NULL CHECK (named IN (0, 1)),
    inputs_json TEXT CHECK (inputs_json IS NULL OR json_valid(inputs_json)),
    outputs_json TEXT CHECK (outputs_json IS NULL OR json_valid(outputs_json)),
    state_json TEXT CHECK (state_json IS NULL OR json_valid(state_json)),
    resource_observation_json TEXT CHECK (resource_observation_json IS NULL OR json_valid(resource_observation_json)),
    resource_observed_at TEXT,
    extra_done_when_json TEXT CHECK (extra_done_when_json IS NULL OR json_valid(extra_done_when_json)),
    error TEXT,
    setup_at TEXT,
    failed_at TEXT,
    cleaned_at TEXT,
    finalized_at TEXT
);

CREATE UNIQUE INDEX task_instances_session_id_instance_name ON task_instances(session_id, instance_name);

-- See node_instance_layers; task instances key their own layer rows by
-- task_instances.id rather than (session_id, node_id) since a dynamic
-- instance has no node_id of its own.
CREATE TABLE task_instance_layers (
    task_instance_id TEXT NOT NULL REFERENCES task_instances(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    effect_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('produced', 'failed', 'cleaned')),
    inputs_json TEXT CHECK (inputs_json IS NULL OR json_valid(inputs_json)),
    locals_json TEXT CHECK (locals_json IS NULL OR json_valid(locals_json)),
    outputs_json TEXT CHECK (outputs_json IS NULL OR json_valid(outputs_json)),
    env_json TEXT CHECK (env_json IS NULL OR json_valid(env_json)),
    heartbeat_ticks INTEGER,
    heartbeat_escalations INTEGER,
    setup_at TEXT,
    failed_at TEXT,
    cleaned_at TEXT,
    error TEXT,
    PRIMARY KEY (task_instance_id, position)
);

CREATE TABLE task_done_when_states (
    task_instance_id TEXT PRIMARY KEY REFERENCES task_instances(id) ON DELETE CASCADE,
    heartbeat_ticks INTEGER NOT NULL DEFAULT 0,
    heartbeat_escalations INTEGER NOT NULL DEFAULT 0,
    last_action TEXT CHECK (last_action IN ('satisfied', 'wait', 'escalate', 'review_required', 'kick')),
    last_fingerprint TEXT,
    last_reason TEXT,
    last_body TEXT,
    escalated_at TEXT,
    escalate_reason TEXT
);

-- Ordered list, replacing task_done_when_states.last_unsatisfied_json: each
-- heartbeat evaluation replaces the whole list (delete + insert in the same
-- write transaction) rather than diffing it.
CREATE TABLE task_done_when_unsatisfied_items (
    task_instance_id TEXT NOT NULL REFERENCES task_instances(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    item TEXT NOT NULL,
    PRIMARY KEY (task_instance_id, position)
);

-- judge_session is a stored fact (the verdict must still read correctly
-- after the judge session is destroyed); judge_workflow is nullable
-- since a judge created via the legacy inline-tasks path has none. The
-- judged side is not stored here at all: task_instance_id's own parent row
-- (session_id, instance_name) is always the judged session/instance, so
-- the contract's DoneWhenJudge carries no separate target field for it.
CREATE TABLE task_done_when_judges (
    task_instance_id TEXT NOT NULL REFERENCES task_instances(id) ON DELETE CASCADE,
    leaf_id TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('approve', 'request_changes')),
    reason TEXT NOT NULL,
    revision TEXT NOT NULL,
    judge_session TEXT NOT NULL,
    judge_workflow TEXT,
    relation TEXT NOT NULL CHECK (relation IN ('self', 'parent', 'child', 'sibling', 'ancestor', 'descendant', 'unrelated')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (task_instance_id, leaf_id)
);

-- (workflow, name) is the population's own domain identity (a workflow's
-- declared population, e.g. its config address plus population name), not
-- the "workflow/name" string built for JSON map keys and in-memory lookups
-- elsewhere — that concatenation is a caller-side artefact the persistence
-- boundary parses back into its two parts, not a stored identity.
CREATE TABLE populations (
    workflow TEXT NOT NULL,
    name TEXT NOT NULL,
    PRIMARY KEY (workflow, name)
);

-- session_name is a recorded fact, not an enforced foreign key to
-- sessions(id): a poll or appearance can accept a member and record the
-- session name it intends to create before that session's own row exists
-- (ApplyPoll/ApplyAppearance run independently of session creation, and
-- mint no id of their own) — unlike sessions.population_workflow/name
-- above, there is no point at which an id could be known here, so this
-- column deliberately keeps naming a session rather than one.
CREATE TABLE population_members (
    workflow TEXT NOT NULL,
    name TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    session_name TEXT,
    generation INTEGER NOT NULL DEFAULT 0,
    accepted_at TEXT,
    last_appearance TEXT,
    last_inbound TEXT,
    tombstoned boolean NOT NULL DEFAULT 0 CHECK (tombstoned IN (0, 1)),
    pending_up boolean NOT NULL DEFAULT 0 CHECK (pending_up IN (0, 1)),
    decision_kind TEXT CHECK (decision_kind IN ('plect.workflow_population.destroy', 'plect.workflow_population.destroy_deferred', 'plect.workflow_population.destroy_dry_run')),
    decision_reason TEXT,
    item_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(item_json)),
    PRIMARY KEY (workflow, name, resource_id),
    FOREIGN KEY (workflow, name) REFERENCES populations(workflow, name) ON DELETE CASCADE
);

-- Ordered list, replacing population_members.last_blockers_json; see
-- task_done_when_unsatisfied_items for the same replace-the-list write
-- pattern.
CREATE TABLE population_member_blockers (
    workflow TEXT NOT NULL,
    name TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    reason TEXT NOT NULL,
    PRIMARY KEY (workflow, name, resource_id, position),
    FOREIGN KEY (workflow, name, resource_id) REFERENCES population_members(workflow, name, resource_id) ON DELETE CASCADE
);

-- Two independent failure streaks of one shape (see
-- Session.ChannelValidationHealth/ChannelDeliveryHealth): validation
-- (checked once, at a dispatcher's build) and delivery (checked per event)
-- run on entirely independent schedules, so a success of one kind must
-- never clear the other's still-open streak. No row exists when a kind has
-- no open failure streak.
CREATE TABLE session_channel_health (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('validation', 'delivery')),
    consecutive_failures INTEGER NOT NULL,
    first_failure_at TEXT NOT NULL,
    last_failure_at TEXT NOT NULL,
    last_channel TEXT,
    last_error TEXT,
    escalated_at TEXT,
    PRIMARY KEY (session_id, kind)
);

-- No foreign key to sessions: a reservation exists for a child session that
-- does not exist yet (it is being created); parent_session_name likewise
-- names a parent that may not exist yet, and NULL rather than a sentinel
-- string means "counted against the virtual root's own cap" (a parentless
-- session, or one whose parent is the "root:" pseudo-parent) — the domain
-- layer's VirtualRootReservationParent sentinel lives only in Go, never in
-- this column.
CREATE TABLE up_reservations (
    child_session_name TEXT PRIMARY KEY,
    parent_session_name TEXT,
    virtual_root boolean NOT NULL DEFAULT 0 CHECK (virtual_root IN (0, 1)),
    pid INTEGER NOT NULL,
    reserved_at TEXT NOT NULL,
    CHECK ((parent_session_name IS NOT NULL) != (virtual_root = 1))
);

-- events/event_cursors key off sessions(id) directly: the per-incarnation
-- event_streams table from PR #463 is retired, because a session row now
-- IS one incarnation (see the sessions table's own comment). A session
-- create mints a new row (new id) for a fresh incarnation; down/up and
-- --force-recreate keep the existing row and its id, so its event history
-- and cursors continue unbroken. Sessions are never deleted, so a
-- destroyed incarnation's events remain readable by its id even though its
-- name has been freed for reuse by a newer row.
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
-- Backs the status-message reader's "latest event of this type for this
-- session" lookup (ORDER BY sequence DESC LIMIT 1) without a per-session
-- table scan.
CREATE INDEX events_session_id_type_sequence_idx ON events(session_id, type, sequence);

-- delivery/tick are at-least-once commitments; heartbeat is a resettable
-- mark (the reactor's own quiet-tick-backoff read position, replacing
-- TickBackoff.LastLogPosition — see sessions.tick_consecutive_unchanged);
-- next_sequence is exclusive and 0 is valid (unconsumed).
CREATE TABLE event_cursors (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('delivery', 'tick', 'heartbeat')),
    next_sequence INTEGER NOT NULL CHECK (next_sequence >= 0),
    PRIMARY KEY (session_id, kind)
);
