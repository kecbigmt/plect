-- name: InsertPersistenceSmoke :one
INSERT INTO persistence_smoke (note, created_at)
VALUES (?, ?)
RETURNING id, note, created_at;

-- Sessions

-- name: UpsertSession :exec
INSERT INTO sessions (
    name, parent_session_name, root_session_name, resource_id, alias,
    workflow, workspace_dir_path, created_at, updated_at, record_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    parent_session_name = excluded.parent_session_name,
    root_session_name = excluded.root_session_name,
    resource_id = excluded.resource_id,
    alias = excluded.alias,
    workflow = excluded.workflow,
    workspace_dir_path = excluded.workspace_dir_path,
    created_at = excluded.created_at,
    updated_at = excluded.updated_at,
    record_json = excluded.record_json;

-- name: GetSession :one
SELECT name, parent_session_name, root_session_name, resource_id, alias,
       workflow, workspace_dir_path, created_at, updated_at, record_json
FROM sessions WHERE name = ?;

-- name: ListSessions :many
SELECT name, parent_session_name, root_session_name, resource_id, alias,
       workflow, workspace_dir_path, created_at, updated_at, record_json
FROM sessions ORDER BY name;

-- name: ListSessionsByAlias :many
SELECT name, parent_session_name, root_session_name, resource_id, alias,
       workflow, workspace_dir_path, created_at, updated_at, record_json
FROM sessions WHERE alias = ? ORDER BY name;

-- name: ListChildSessionNames :many
SELECT name FROM sessions WHERE parent_session_name = ? ORDER BY name;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE name = ?;

-- name: SessionParent :one
SELECT parent_session_name FROM sessions WHERE name = ?;

-- name: CountSessionsNamed :one
SELECT COUNT(*) FROM sessions WHERE name = ?;

-- Task instances

-- name: InsertTaskInstance :exec
INSERT INTO task_instances (
    session_name, instance_name, task_id, scope, status, sequence, dynamic,
    resource, named_instance, record_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTaskInstances :many
SELECT session_name, instance_name, task_id, scope, status, sequence, dynamic,
       resource, named_instance, record_json
FROM task_instances WHERE session_name = ? ORDER BY instance_name;

-- name: DeleteTaskInstancesForSession :exec
DELETE FROM task_instances WHERE session_name = ?;

-- Task done_when

-- name: InsertTaskDoneWhen :exec
INSERT INTO task_done_when (
    session_name, instance_name, heartbeat_ticks, heartbeat_escalations,
    last_action, last_fingerprint, last_reason, last_unsatisfied_json,
    last_body, escalated_at, escalate_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTaskDoneWhen :many
SELECT session_name, instance_name, heartbeat_ticks, heartbeat_escalations,
       last_action, last_fingerprint, last_reason, last_unsatisfied_json,
       last_body, escalated_at, escalate_reason
FROM task_done_when WHERE session_name = ?;

-- Task done_when judges

-- name: InsertTaskDoneWhenJudge :exec
INSERT INTO task_done_when_judges (
    session_name, instance_name, leaf_id, action, reason, revision,
    target_session, target_instance, reviewer_session, reviewer_workflow,
    relation, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTaskDoneWhenJudges :many
SELECT session_name, instance_name, leaf_id, action, reason, revision,
       target_session, target_instance, reviewer_session, reviewer_workflow,
       relation, created_at
FROM task_done_when_judges WHERE session_name = ?;

-- Populations

-- name: GetPopulation :one
SELECT population_key, workflow, name FROM populations WHERE population_key = ?;

-- name: UpsertPopulation :exec
INSERT INTO populations (population_key, workflow, name) VALUES (?, ?, ?)
ON CONFLICT(population_key) DO UPDATE SET workflow = excluded.workflow, name = excluded.name;

-- name: ListPopulationMembers :many
SELECT population_key, resource_id, session_name, generation, accepted_at,
       last_appearance, last_inbound, tombstoned, pending_up, last_decision,
       item_json, last_blockers_json
FROM population_members WHERE population_key = ? ORDER BY resource_id;

-- name: DeletePopulationMembersForPopulation :exec
DELETE FROM population_members WHERE population_key = ?;

-- name: InsertPopulationMember :exec
INSERT INTO population_members (
    population_key, resource_id, session_name, generation, accepted_at,
    last_appearance, last_inbound, tombstoned, pending_up, last_decision,
    item_json, last_blockers_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- Up-slot reservations

-- name: ListUpReservations :many
SELECT child_session_name, parent_name, pid, reserved_at FROM up_reservations ORDER BY child_session_name;

-- name: UpsertUpReservation :exec
INSERT INTO up_reservations (child_session_name, parent_name, pid, reserved_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(child_session_name) DO UPDATE SET
    parent_name = excluded.parent_name,
    pid = excluded.pid,
    reserved_at = excluded.reserved_at;

-- name: DeleteUpReservation :exec
DELETE FROM up_reservations WHERE child_session_name = ?;
