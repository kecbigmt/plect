-- schema.sql is the hand-edited declarative authority for the database
-- structure described in docs/design/sqlite-persistence.md. Generate
-- migrations from it with Atlas Community Edition; do not hand-write
-- migration SQL against a structural change already captured here.
--
-- The bootstrap slice's persistence_smoke table proves the schema.sql ->
-- Atlas -> goose -> sqlite pipeline and the sqlc -> Go pipeline end to end;
-- it stays alongside the domain tables below rather than being retired,
-- since the toolchain slice's own tests still exercise it.
CREATE TABLE persistence_smoke (
    id INTEGER PRIMARY KEY,
    note TEXT NOT NULL,
    created_at TEXT NOT NULL
);

-- Runtime state tables (issue #433): sessions, task instances, done_when /
-- judge state, populations, and up-slot reservations. Event tables
-- (event_streams, events, event_consumer_positions, event_watermarks,
-- session_tombstones, pending_deliveries) belong to a later slice.
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

-- Session-scoped task instances. done_when counters/fingerprints and judge
-- verdicts are split into their own tables below rather than folded into
-- record_json, so a judge verdict is never duplicated between two
-- authorities.
CREATE TABLE task_instances (
    session_name TEXT NOT NULL REFERENCES sessions(name) ON DELETE CASCADE,
    instance_name TEXT NOT NULL,
    task_id TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL,
    status TEXT NOT NULL,
    sequence INTEGER NOT NULL DEFAULT 0,
    dynamic INTEGER NOT NULL DEFAULT 0,
    resource TEXT NOT NULL DEFAULT '',
    named_instance TEXT NOT NULL DEFAULT '',
    record_json TEXT NOT NULL,
    PRIMARY KEY (session_name, instance_name)
);

CREATE TABLE task_done_when (
    session_name TEXT NOT NULL,
    instance_name TEXT NOT NULL,
    heartbeat_ticks INTEGER NOT NULL DEFAULT 0,
    heartbeat_escalations INTEGER NOT NULL DEFAULT 0,
    last_action TEXT NOT NULL DEFAULT '',
    last_fingerprint TEXT NOT NULL DEFAULT '',
    last_reason TEXT NOT NULL DEFAULT '',
    last_unsatisfied_json TEXT NOT NULL DEFAULT '[]',
    last_body TEXT NOT NULL DEFAULT '',
    escalated_at TEXT NOT NULL DEFAULT '',
    escalate_reason TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_name, instance_name),
    FOREIGN KEY (session_name, instance_name) REFERENCES task_instances(session_name, instance_name) ON DELETE CASCADE
);

-- reviewer_session / target_session / target_instance are stored facts
-- (the verdict must still read correctly after the reviewer session or the
-- tree shape changes), not references derivable from the primary key.
CREATE TABLE task_done_when_judges (
    session_name TEXT NOT NULL,
    instance_name TEXT NOT NULL,
    leaf_id TEXT NOT NULL,
    action TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    revision TEXT NOT NULL DEFAULT '',
    target_session TEXT NOT NULL DEFAULT '',
    target_instance TEXT NOT NULL DEFAULT '',
    reviewer_session TEXT NOT NULL DEFAULT '',
    reviewer_workflow TEXT NOT NULL DEFAULT '',
    relation TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (session_name, instance_name, leaf_id),
    FOREIGN KEY (session_name, instance_name) REFERENCES task_instances(session_name, instance_name) ON DELETE CASCADE
);

CREATE TABLE populations (
    population_key TEXT PRIMARY KEY,
    workflow TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT ''
);

-- session_name is a recorded fact, not an enforced foreign key: a poll or
-- appearance can accept a member and record the session name it intends to
-- create before that session's own row exists (ApplyPoll/ApplyAppearance
-- run independently of session creation), so a hard reference would reject
-- a legitimate, momentarily-forward-pointing write.
CREATE TABLE population_members (
    population_key TEXT NOT NULL REFERENCES populations(population_key) ON DELETE CASCADE,
    resource_id TEXT NOT NULL,
    session_name TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 0,
    accepted_at TEXT NOT NULL DEFAULT '',
    last_appearance TEXT NOT NULL DEFAULT '',
    last_inbound TEXT NOT NULL DEFAULT '',
    tombstoned INTEGER NOT NULL DEFAULT 0,
    pending_up INTEGER NOT NULL DEFAULT 0,
    last_decision TEXT NOT NULL DEFAULT '',
    item_json TEXT NOT NULL DEFAULT '{}',
    last_blockers_json TEXT NOT NULL DEFAULT '[]',
    PRIMARY KEY (population_key, resource_id)
);

-- No foreign key to sessions: a reservation exists for a child session that
-- does not exist yet (it is being created).
CREATE TABLE up_reservations (
    child_session_name TEXT PRIMARY KEY,
    parent_name TEXT NOT NULL DEFAULT '',
    pid INTEGER NOT NULL,
    reserved_at TEXT NOT NULL
);
