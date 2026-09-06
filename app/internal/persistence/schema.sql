-- schema.sql is the hand-edited declarative authority for the database
-- structure described in docs/design/sqlite-persistence.md. Generate
-- migrations from it with Atlas Community Edition; do not hand-write
-- migration SQL against a structural change already captured here.
--
-- Runtime state tables: sessions, node-instance and task-instance state,
-- done_when / judge state, populations, and up-slot reservations. Event
-- tables (event_streams, events, event_consumer_positions,
-- event_watermarks, session_tombstones, pending_deliveries) belong to a
-- later slice.
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
    name TEXT PRIMARY KEY,
    parent_session_name TEXT REFERENCES sessions(name) ON DELETE SET NULL,
    root_session_name TEXT REFERENCES sessions(name) ON DELETE SET NULL,
    resource_id TEXT,
    alias TEXT,
    workflow TEXT NOT NULL,
    workspace_dir TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    record_json TEXT NOT NULL,
    CHECK (NOT (parent_session_name IS NOT NULL AND root_session_name IS NOT NULL)),
    CHECK (root_session_name IS NULL OR root_session_name <> name)
);

CREATE INDEX sessions_alias_idx ON sessions(alias);
CREATE INDEX sessions_parent_idx ON sessions(parent_session_name);

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
CREATE TABLE node_instances (
    session_name TEXT NOT NULL REFERENCES sessions(name) ON DELETE CASCADE,
    node_id TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('session', 'run')),
    status TEXT NOT NULL CHECK (status IN ('produced', 'failed', 'cleaned')),
    sequence INTEGER NOT NULL,
    finalized_at TEXT,
    record_json TEXT NOT NULL,
    PRIMARY KEY (session_name, node_id)
);

-- A dynamic instance's id is a ULID minted once when the row is first
-- created (task setup) and preserved by every later Put/Update that still
-- names the same (session_name, instance_name) — only a cleanup (the row
-- disappearing from a write's Tasks map) followed by a new setup mints a
-- fresh one. done_when counters/fingerprints and judge verdicts are split
-- into their own tables below (keyed by this id) rather than folded into
-- record_json, so a judge verdict is never duplicated between two
-- authorities. A static node_instances row's own done_when (rare, and not
-- relationally queried) stays embedded in its record_json instead.
--
-- named replaces a would-be duplicated instance-identity string: Name (the
-- `--name` a caller gave the instance) is always either empty or exactly
-- instance_name, so which case applies is the only bit that does not
-- already live in instance_name itself.
CREATE TABLE task_instances (
    id TEXT PRIMARY KEY,
    session_name TEXT NOT NULL REFERENCES sessions(name) ON DELETE CASCADE,
    instance_name TEXT NOT NULL,
    task_id TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('session', 'run')),
    status TEXT NOT NULL CHECK (status IN ('produced', 'failed', 'cleaned')),
    sequence INTEGER NOT NULL,
    resource TEXT,
    named boolean NOT NULL CHECK (named IN (0, 1)),
    finalized_at TEXT,
    record_json TEXT NOT NULL
);

CREATE UNIQUE INDEX task_instances_session_name_instance_name ON task_instances(session_name, instance_name);

CREATE TABLE task_done_when_states (
    task_instance_id TEXT PRIMARY KEY REFERENCES task_instances(id) ON DELETE CASCADE,
    heartbeat_ticks INTEGER NOT NULL DEFAULT 0,
    heartbeat_escalations INTEGER NOT NULL DEFAULT 0,
    last_action TEXT CHECK (last_action IN ('satisfied', 'wait', 'escalate', 'review_required', 'kick')),
    last_fingerprint TEXT,
    last_reason TEXT,
    last_unsatisfied_json TEXT NOT NULL DEFAULT '[]',
    last_body TEXT,
    escalated_at TEXT,
    escalate_reason TEXT
);

-- judge_session is a stored fact (the verdict must still read correctly
-- after the reviewer session is destroyed); judge_workflow is nullable
-- since a reviewer created via the legacy inline-tasks path has none. The
-- judged side is not stored here at all: task_instance_id's own parent row
-- (session_name, instance_name) is always the judged session/instance, so
-- the persistence boundary derives DoneWhenJudge.TargetSession/Instance
-- from that join rather than duplicating it as columns.
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

-- population_members is the single source for a session's population
-- membership: Session.Population is derived at read time by joining on
-- session_name rather than duplicated into sessions.record_json, since the
-- one write path that ever sets a session's Population field already
-- verifies (via the "owned by another lifecycle authority" conflict check)
-- that it agrees with the population this row will go on to record —
-- there is no code path where the two could legitimately disagree.
-- session_name is a recorded fact, not an enforced foreign key: a poll or
-- appearance can accept a member and record the session name it intends to
-- create before that session's own row exists (ApplyPoll/ApplyAppearance
-- run independently of session creation), so a hard reference would reject
-- a legitimate, momentarily-forward-pointing write.
--
-- decision_kind/decision_reason replace a single last_decision string that
-- packed an event-type constant and an optional free-text reason into one
-- "kind:reason" (or bare "kind") value; decision_kind's CHECK set is
-- contracts/event's TypeWorkflowPopulationDestroy(/Deferred/DryRun).
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
    item_json TEXT NOT NULL DEFAULT '{}',
    last_blockers_json TEXT NOT NULL DEFAULT '[]',
    PRIMARY KEY (workflow, name, resource_id),
    FOREIGN KEY (workflow, name) REFERENCES populations(workflow, name) ON DELETE CASCADE
);

-- No foreign key to sessions: a reservation exists for a child session that
-- does not exist yet (it is being created).
CREATE TABLE up_reservations (
    child_session_name TEXT PRIMARY KEY,
    parent_name TEXT NOT NULL,
    pid INTEGER NOT NULL,
    reserved_at TEXT NOT NULL
);
