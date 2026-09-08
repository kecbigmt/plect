-- schema.sql is the hand-edited declarative authority for the database
-- structure described in docs/design/sqlite-persistence.md. Generate
-- migrations from it with Atlas Community Edition; do not hand-write
-- migration SQL against a structural change already captured here.
--
-- Runtime, event, and retry-state tables live here. A destroyed sessions row
-- is the tombstone for that incarnation.
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
    inputs_json TEXT CHECK (inputs_json IS NULL OR json_valid(inputs_json)),
    -- health_* is a derived runtime observation, not a lifecycle status;
    -- see docs/design/sqlite-persistence.md.
    health_last_checked_at TEXT,
    health_last_activity_at TEXT,
    health_last_fingerprint TEXT,
    health_last_state TEXT CHECK (health_last_state IS NULL OR health_last_state IN ('healthy', 'unhealthy', 'stalled', 'undeclared')),
    health_last_reason TEXT,
    health_last_notified_at TEXT,
    health_notify_count INTEGER,
    -- Tick backoff's own log-position watermark lives in event_cursors'
    -- `heartbeat` kind instead of a column here; see that table.
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

-- Unique only among live rows; a destroyed session's name is free to reuse.
CREATE UNIQUE INDEX sessions_live_name ON sessions(name) WHERE status <> 'destroyed';
CREATE INDEX sessions_alias_idx ON sessions(alias);
CREATE INDEX sessions_parent_idx ON sessions(parent_session_id);

-- A cap-refusal streak is scoped to a session incarnation, not its reusable
-- name. An empty fingerprint is represented by no row.
CREATE TABLE chain_attempts (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    instance TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    generation TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    PRIMARY KEY (session_id, instance, chain_id, generation)
);

-- Failed provider subscribe/unsubscribe intents await an opportunistic retry.
CREATE TABLE subscription_retries (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('subscribe', 'unsubscribe')),
    resource TEXT NOT NULL,
    PRIMARY KEY (session_id, action, resource)
);

-- Static workflow-DAG nodes' identity only; see node_executions below.
CREATE TABLE node_instances (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL,
    PRIMARY KEY (session_id, node_id)
);

-- One row per setup attempt; see node_executions_one_unreleased_idx below.
CREATE TABLE node_executions (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    node_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    task_id TEXT,
    name TEXT,
    scope TEXT NOT NULL CHECK (scope IN ('session', 'run')),
    status TEXT NOT NULL CHECK (status IN ('produced', 'failed', 'cleaned')),
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
    FOREIGN KEY (session_id, node_id) REFERENCES node_instances(session_id, node_id) ON DELETE CASCADE
);

CREATE INDEX node_executions_session_node_idx ON node_executions(session_id, node_id, sequence);
CREATE UNIQUE INDEX node_executions_one_unreleased_idx ON node_executions(session_id, node_id) WHERE status <> 'cleaned';

-- One row per layer of one execution's nested effect chain.
CREATE TABLE node_execution_layers (
    execution_id TEXT NOT NULL REFERENCES node_executions(id) ON DELETE CASCADE,
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
    PRIMARY KEY (execution_id, position)
);

-- execution_id is the dependent (released first), depends_on_execution_id the prerequisite.
CREATE TABLE node_execution_dependencies (
    execution_id TEXT NOT NULL REFERENCES node_executions(id) ON DELETE CASCADE,
    depends_on_execution_id TEXT NOT NULL REFERENCES node_executions(id) ON DELETE CASCADE,
    PRIMARY KEY (execution_id, depends_on_execution_id),
    CHECK (execution_id <> depends_on_execution_id)
);

CREATE INDEX node_execution_dependencies_depends_on_idx ON node_execution_dependencies(depends_on_execution_id);

-- id is preserved across an update; only a cleanup+setup mints a fresh one.
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

-- See node_instance_layers; keyed by task_instances.id.
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

-- Ordered list; each heartbeat evaluation replaces it wholesale rather than diffing.
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

-- (workflow, name) is the population's own domain identity, not the
-- "workflow/name" map-key string built elsewhere (a caller-side artefact
-- the persistence boundary parses back into its two parts).
CREATE TABLE populations (
    workflow TEXT NOT NULL,
    name TEXT NOT NULL,
    PRIMARY KEY (workflow, name)
);

-- session_name is a recorded fact, not a foreign key: a poll/appearance can
-- accept a member and record its intended session name before that session's own row exists.
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

-- Ordered list; same replace-wholesale pattern as task_done_when_unsatisfied_items.
CREATE TABLE population_member_blockers (
    workflow TEXT NOT NULL,
    name TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    reason TEXT NOT NULL,
    PRIMARY KEY (workflow, name, resource_id, position),
    FOREIGN KEY (workflow, name, resource_id) REFERENCES population_members(workflow, name, resource_id) ON DELETE CASCADE
);

-- Two independent failure streaks (validation, delivery); no row when open.
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

-- events/event_cursors key off sessions(id) directly: a row now is one incarnation.
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
-- Backs the status-message reader's latest-event-of-this-type lookup.
CREATE INDEX events_session_id_type_sequence_idx ON events(session_id, type, sequence);

-- heartbeat is the reactor's resettable quiet-backoff position; next_sequence is exclusive.
CREATE TABLE event_cursors (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('delivery', 'tick', 'heartbeat', 'forward')),
    next_sequence INTEGER NOT NULL CHECK (next_sequence >= 0),
    PRIMARY KEY (session_id, kind)
);
