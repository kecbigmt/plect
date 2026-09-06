-- Sessions

-- name: UpsertSession :exec
INSERT INTO sessions (
    name, parent_session_name, root_session_name, resource_id, alias,
    workflow, workspace_dir, created_at, updated_at, record_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    parent_session_name = excluded.parent_session_name,
    root_session_name = excluded.root_session_name,
    resource_id = excluded.resource_id,
    alias = excluded.alias,
    workflow = excluded.workflow,
    workspace_dir = excluded.workspace_dir,
    created_at = excluded.created_at,
    updated_at = excluded.updated_at,
    record_json = excluded.record_json;

-- name: GetSession :one
SELECT name, parent_session_name, root_session_name, resource_id, alias,
       workflow, workspace_dir, created_at, updated_at, record_json
FROM sessions WHERE name = ?;

-- name: ListSessions :many
SELECT name, parent_session_name, root_session_name, resource_id, alias,
       workflow, workspace_dir, created_at, updated_at, record_json
FROM sessions ORDER BY name;

-- name: ListSessionsByAlias :many
SELECT name, parent_session_name, root_session_name, resource_id, alias,
       workflow, workspace_dir, created_at, updated_at, record_json
FROM sessions WHERE alias = ? ORDER BY name;

-- name: ListChildSessionNames :many
SELECT name FROM sessions WHERE parent_session_name = ? ORDER BY name;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE name = ?;

-- name: SessionParent :one
SELECT parent_session_name FROM sessions WHERE name = ?;

-- name: CountSessionsNamed :one
SELECT COUNT(*) FROM sessions WHERE name = ?;

-- Workflow nodes (static; Session.Tasks entries with Dynamic == false)

-- name: InsertNodeInstance :exec
INSERT INTO node_instances (
    session_name, node_id, scope, status, sequence, finalized_at, record_json
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListNodeInstances :many
SELECT session_name, node_id, scope, status, sequence, finalized_at, record_json
FROM node_instances WHERE session_name = ? ORDER BY node_id;

-- name: DeleteNodeInstancesForSession :exec
DELETE FROM node_instances WHERE session_name = ?;

-- Task instances (dynamic; Session.Tasks entries with Dynamic == true)

-- name: ListTaskInstances :many
SELECT id, session_name, instance_name, task_id, scope, status, sequence,
       resource, named, finalized_at, record_json
FROM task_instances WHERE session_name = ? ORDER BY instance_name;

-- UpsertTaskInstance preserves the existing id when (session_name,
-- instance_name) already has a row (an ordinary Put/Update of a live
-- instance) and keeps the freshly minted candidate id only when inserting
-- a genuinely new row; RETURNING id reports whichever one now applies.
-- name: UpsertTaskInstance :one
INSERT INTO task_instances (
    id, session_name, instance_name, task_id, scope, status, sequence,
    resource, named, finalized_at, record_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_name, instance_name) DO UPDATE SET
    task_id = excluded.task_id,
    scope = excluded.scope,
    status = excluded.status,
    sequence = excluded.sequence,
    resource = excluded.resource,
    named = excluded.named,
    finalized_at = excluded.finalized_at,
    record_json = excluded.record_json
RETURNING id;

-- name: DeleteTaskInstanceByName :exec
DELETE FROM task_instances WHERE session_name = ? AND instance_name = ?;

-- name: DeleteTaskInstancesForSession :exec
DELETE FROM task_instances WHERE session_name = ?;

-- Task done_when states

-- name: InsertTaskDoneWhenState :exec
INSERT INTO task_done_when_states (
    task_instance_id, heartbeat_ticks, heartbeat_escalations,
    last_action, last_fingerprint, last_reason, last_unsatisfied_json,
    last_body, escalated_at, escalate_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: DeleteTaskDoneWhenStateByInstanceID :exec
DELETE FROM task_done_when_states WHERE task_instance_id = ?;

-- name: ListTaskDoneWhenStatesForSession :many
SELECT s.task_instance_id, s.heartbeat_ticks, s.heartbeat_escalations,
       s.last_action, s.last_fingerprint, s.last_reason, s.last_unsatisfied_json,
       s.last_body, s.escalated_at, s.escalate_reason
FROM task_done_when_states s
JOIN task_instances t ON t.id = s.task_instance_id
WHERE t.session_name = ?;

-- Task done_when judges

-- name: InsertTaskDoneWhenJudge :exec
INSERT INTO task_done_when_judges (
    task_instance_id, leaf_id, action, reason, revision,
    judge_session, judge_workflow, relation, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: DeleteTaskDoneWhenJudgesByInstanceID :exec
DELETE FROM task_done_when_judges WHERE task_instance_id = ?;

-- name: ListTaskDoneWhenJudgesForSession :many
SELECT j.task_instance_id, j.leaf_id, j.action, j.reason, j.revision,
       j.judge_session, j.judge_workflow, j.relation, j.created_at,
       t.session_name AS target_session, t.instance_name AS target_instance
FROM task_done_when_judges j
JOIN task_instances t ON t.id = j.task_instance_id
WHERE t.session_name = ?;

-- Populations

-- name: GetPopulation :one
SELECT workflow, name FROM populations WHERE workflow = ? AND name = ?;

-- name: UpsertPopulation :exec
INSERT INTO populations (workflow, name) VALUES (?, ?)
ON CONFLICT(workflow, name) DO NOTHING;

-- name: ListPopulationMembers :many
SELECT workflow, name, resource_id, session_name, generation, accepted_at,
       last_appearance, last_inbound, tombstoned, pending_up,
       decision_kind, decision_reason, item_json, last_blockers_json
FROM population_members WHERE workflow = ? AND name = ? ORDER BY resource_id;

-- name: DeletePopulationMembersForPopulation :exec
DELETE FROM population_members WHERE workflow = ? AND name = ?;

-- name: InsertPopulationMember :exec
INSERT INTO population_members (
    workflow, name, resource_id, session_name, generation, accepted_at,
    last_appearance, last_inbound, tombstoned, pending_up,
    decision_kind, decision_reason, item_json, last_blockers_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

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
