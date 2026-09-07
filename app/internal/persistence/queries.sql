-- Sessions

-- name: InsertSession :exec
INSERT INTO sessions (
    id, name, status, destroyed_at, parent_session_id, root_session_id,
    resource_id, alias, workflow, workspace_dir,
    population_workflow, population_name, inputs_json,
    health_last_checked_at, health_last_activity_at, health_last_fingerprint,
    health_last_state, health_last_reason, health_last_notified_at, health_notify_count,
    tick_consecutive_unchanged, tick_last_fingerprint, last_tick_at,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSessionByID :exec
UPDATE sessions SET
    name = ?,
    status = ?,
    destroyed_at = ?,
    parent_session_id = ?,
    root_session_id = ?,
    resource_id = ?,
    alias = ?,
    workflow = ?,
    workspace_dir = ?,
    population_workflow = ?,
    population_name = ?,
    inputs_json = ?,
    health_last_checked_at = ?,
    health_last_activity_at = ?,
    health_last_fingerprint = ?,
    health_last_state = ?,
    health_last_reason = ?,
    health_last_notified_at = ?,
    health_notify_count = ?,
    tick_consecutive_unchanged = ?,
    tick_last_fingerprint = ?,
    last_tick_at = ?,
    updated_at = ?
WHERE id = ?;

-- name: GetLiveSession :one
SELECT id, name, status, destroyed_at, parent_session_id, root_session_id,
       resource_id, alias, workflow, workspace_dir,
       population_workflow, population_name, inputs_json,
       health_last_checked_at, health_last_activity_at, health_last_fingerprint,
       health_last_state, health_last_reason, health_last_notified_at, health_notify_count,
       tick_consecutive_unchanged, tick_last_fingerprint, last_tick_at,
       created_at, updated_at
FROM sessions WHERE name = ? AND status <> 'destroyed';

-- name: ListLiveSessions :many
SELECT id, name, status, destroyed_at, parent_session_id, root_session_id,
       resource_id, alias, workflow, workspace_dir,
       population_workflow, population_name, inputs_json,
       health_last_checked_at, health_last_activity_at, health_last_fingerprint,
       health_last_state, health_last_reason, health_last_notified_at, health_notify_count,
       tick_consecutive_unchanged, tick_last_fingerprint, last_tick_at,
       created_at, updated_at
FROM sessions WHERE status <> 'destroyed' ORDER BY name;

-- name: ListLiveSessionsByAlias :many
SELECT id, name, status, destroyed_at, parent_session_id, root_session_id,
       resource_id, alias, workflow, workspace_dir,
       population_workflow, population_name, inputs_json,
       health_last_checked_at, health_last_activity_at, health_last_fingerprint,
       health_last_state, health_last_reason, health_last_notified_at, health_notify_count,
       tick_consecutive_unchanged, tick_last_fingerprint, last_tick_at,
       created_at, updated_at
FROM sessions WHERE alias = ? AND status <> 'destroyed' ORDER BY name;

-- name: ListLiveChildSessionNames :many
SELECT name FROM sessions WHERE parent_session_id = ? AND status <> 'destroyed' ORDER BY name;

-- name: SessionIDByLiveName :one
SELECT id FROM sessions WHERE name = ? AND status <> 'destroyed';

-- name: SessionNameByID :one
SELECT name FROM sessions WHERE id = ?;

-- name: LiveSessionParentID :one
SELECT parent_session_id FROM sessions WHERE id = ?;

-- name: CountLiveSessionsNamed :one
SELECT COUNT(*) FROM sessions WHERE name = ? AND status <> 'destroyed';

-- name: ListSessionIDsByName :many
SELECT id FROM sessions WHERE name = ? ORDER BY created_at ASC;

-- name: DeleteSessionByID :exec
-- node_instances/task_instances/session_channel_health/event_cursors all
-- cascade from this; events does not (see its own table comment), so a
-- caller must delete those first.
DELETE FROM sessions WHERE id = ?;

-- name: ClearParentReferencesToID :exec
-- parent_session_id/root_session_id are ON DELETE NO ACTION: a caller must
-- sever every inbound reference to an id before DeleteSessionByID, or the
-- delete is rejected outright.
UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id = ?;

-- name: ClearRootReferencesToID :exec
UPDATE sessions SET root_session_id = NULL WHERE root_session_id = ?;

-- name: SessionEverExistedByName :one
SELECT EXISTS(SELECT 1 FROM sessions WHERE name = ?);

-- name: ListEverSessionNames :many
SELECT DISTINCT name FROM sessions ORDER BY name;

-- Workflow nodes (static; Session.Nodes entries)

-- name: EnsureNodeInstance :exec
INSERT INTO node_instances (session_id, node_id)
VALUES (?, ?)
ON CONFLICT (session_id, node_id) DO NOTHING;

-- name: DeleteNodeInstancesForSession :exec
DELETE FROM node_instances WHERE session_id = ?;

-- name: DeleteReleasedNodeInstance :execrows
DELETE FROM node_instances
WHERE session_id = ? AND node_id = ?
AND NOT EXISTS (
    SELECT 1 FROM node_executions
    WHERE node_executions.session_id = node_instances.session_id
      AND node_executions.node_id = node_instances.node_id
      AND node_executions.status <> 'cleaned'
);

-- name: CurrentNodeExecution :one
SELECT id, session_id, node_id, sequence, task_id, name, scope, status, resource,
       inputs_json, outputs_json, state_json,
       resource_observation_json, resource_observed_at, done_when_json,
       extra_done_when_json, error, setup_at,
       failed_at, cleaned_at, finalized_at
FROM node_executions WHERE session_id = ? AND node_id = ? AND status <> 'cleaned';

-- name: NodeExecutionByID :one
SELECT id, session_id, node_id, sequence, task_id, name, scope, status, resource,
       inputs_json, outputs_json, state_json,
       resource_observation_json, resource_observed_at, done_when_json,
       extra_done_when_json, error, setup_at,
       failed_at, cleaned_at, finalized_at
FROM node_executions WHERE id = ?;

-- name: InsertNodeExecution :one
INSERT INTO node_executions (
    id, session_id, node_id, sequence, task_id, name, scope, status, resource,
    inputs_json, outputs_json, state_json,
    resource_observation_json, resource_observed_at, done_when_json,
    extra_done_when_json, error, setup_at,
    failed_at, cleaned_at, finalized_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateNodeExecution :exec
UPDATE node_executions SET
    sequence = ?, task_id = ?, name = ?, scope = ?, status = ?, resource = ?,
    inputs_json = ?, outputs_json = ?, state_json = ?,
    resource_observation_json = ?, resource_observed_at = ?, done_when_json = ?,
    extra_done_when_json = ?, error = ?,
    setup_at = ?, failed_at = ?, cleaned_at = ?, finalized_at = ?
WHERE id = ?;

-- name: ListCurrentNodeExecutions :many
SELECT ne.id, ne.session_id, ne.node_id, ne.sequence, ne.task_id, ne.name,
       ne.scope, ne.status, ne.resource, ne.inputs_json,
       ne.outputs_json, ne.state_json, ne.resource_observation_json,
       ne.resource_observed_at, ne.done_when_json, ne.extra_done_when_json,
       ne.error, ne.setup_at, ne.failed_at,
       ne.cleaned_at, ne.finalized_at
FROM node_executions ne
WHERE ne.session_id = ?
AND NOT EXISTS (
    SELECT 1 FROM node_executions newer
    WHERE newer.session_id = ne.session_id AND newer.node_id = ne.node_id
      AND (newer.sequence > ne.sequence OR (newer.sequence = ne.sequence AND newer.id > ne.id))
)
ORDER BY ne.node_id;

-- name: DeleteNodeExecutionLayers :exec
DELETE FROM node_execution_layers WHERE execution_id = ?;

-- name: InsertNodeExecutionLayer :exec
INSERT INTO node_execution_layers (
    execution_id, position, effect_id, status, inputs_json, locals_json,
    outputs_json, env_json, heartbeat_ticks, heartbeat_escalations,
    setup_at, failed_at, cleaned_at, error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListNodeExecutionLayersForSession :many
SELECT nel.execution_id, nel.position, nel.effect_id, nel.status,
       nel.inputs_json, nel.locals_json, nel.outputs_json, nel.env_json,
       nel.heartbeat_ticks, nel.heartbeat_escalations, nel.setup_at,
       nel.failed_at, nel.cleaned_at, nel.error
FROM node_execution_layers nel
INNER JOIN node_executions ne ON ne.id = nel.execution_id
WHERE ne.session_id = ?
ORDER BY nel.execution_id, nel.position;

-- name: DeleteNodeExecutionDependencies :exec
DELETE FROM node_execution_dependencies WHERE execution_id = ?;

-- name: InsertNodeExecutionDependency :exec
INSERT INTO node_execution_dependencies (execution_id, depends_on_execution_id)
VALUES (?, ?)
ON CONFLICT (execution_id, depends_on_execution_id) DO NOTHING;

-- name: ListNodeExecutionDependenciesForSession :many
SELECT dependent.node_id AS node_id, ned.execution_id AS execution_id, prereq.node_id AS depends_on_node_id
FROM node_execution_dependencies ned
INNER JOIN node_executions dependent ON dependent.id = ned.execution_id
INNER JOIN node_executions prereq ON prereq.id = ned.depends_on_execution_id
WHERE dependent.session_id = ?;

-- Task instances (dynamic; Session.Tasks entries)

-- name: ListTaskInstances :many
SELECT id, session_id, instance_name, task_id, scope, status, sequence,
       resource, named, inputs_json, outputs_json, state_json,
       resource_observation_json, resource_observed_at, extra_done_when_json,
       error, setup_at, failed_at, cleaned_at, finalized_at
FROM task_instances WHERE session_id = ? ORDER BY instance_name;

-- name: UpsertTaskInstance :one
INSERT INTO task_instances (
    id, session_id, instance_name, task_id, scope, status, sequence,
    resource, named, inputs_json, outputs_json, state_json,
    resource_observation_json, resource_observed_at, extra_done_when_json,
    error, setup_at, failed_at, cleaned_at, finalized_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, instance_name) DO UPDATE SET
    task_id = excluded.task_id,
    scope = excluded.scope,
    status = excluded.status,
    sequence = excluded.sequence,
    resource = excluded.resource,
    named = excluded.named,
    inputs_json = excluded.inputs_json,
    outputs_json = excluded.outputs_json,
    state_json = excluded.state_json,
    resource_observation_json = excluded.resource_observation_json,
    resource_observed_at = excluded.resource_observed_at,
    extra_done_when_json = excluded.extra_done_when_json,
    error = excluded.error,
    setup_at = excluded.setup_at,
    failed_at = excluded.failed_at,
    cleaned_at = excluded.cleaned_at,
    finalized_at = excluded.finalized_at
RETURNING id;

-- name: DeleteTaskInstanceByName :exec
DELETE FROM task_instances WHERE session_id = ? AND instance_name = ?;

-- name: DeleteTaskInstanceLayersByInstanceID :exec
DELETE FROM task_instance_layers WHERE task_instance_id = ?;

-- name: InsertTaskInstanceLayer :exec
INSERT INTO task_instance_layers (
    task_instance_id, position, effect_id, status, inputs_json, locals_json,
    outputs_json, env_json, heartbeat_ticks, heartbeat_escalations,
    setup_at, failed_at, cleaned_at, error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTaskInstanceLayersForSession :many
SELECT l.task_instance_id, l.position, l.effect_id, l.status, l.inputs_json,
       l.locals_json, l.outputs_json, l.env_json, l.heartbeat_ticks,
       l.heartbeat_escalations, l.setup_at, l.failed_at, l.cleaned_at, l.error
FROM task_instance_layers l
JOIN task_instances t ON t.id = l.task_instance_id
WHERE t.session_id = ?
ORDER BY l.task_instance_id, l.position;

-- Task done_when states

-- name: InsertTaskDoneWhenState :exec
INSERT INTO task_done_when_states (
    task_instance_id, heartbeat_ticks, heartbeat_escalations,
    last_action, last_fingerprint, last_reason,
    last_body, escalated_at, escalate_reason
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: DeleteTaskDoneWhenStateByInstanceID :exec
DELETE FROM task_done_when_states WHERE task_instance_id = ?;

-- name: ListTaskDoneWhenStatesForSession :many
SELECT s.task_instance_id, s.heartbeat_ticks, s.heartbeat_escalations,
       s.last_action, s.last_fingerprint, s.last_reason,
       s.last_body, s.escalated_at, s.escalate_reason
FROM task_done_when_states s
JOIN task_instances t ON t.id = s.task_instance_id
WHERE t.session_id = ?;

-- name: InsertTaskDoneWhenUnsatisfiedItem :exec
INSERT INTO task_done_when_unsatisfied_items (task_instance_id, position, item)
VALUES (?, ?, ?);

-- name: DeleteTaskDoneWhenUnsatisfiedItemsByInstanceID :exec
DELETE FROM task_done_when_unsatisfied_items WHERE task_instance_id = ?;

-- name: ListTaskDoneWhenUnsatisfiedItemsForSession :many
SELECT i.task_instance_id, i.position, i.item
FROM task_done_when_unsatisfied_items i
JOIN task_instances t ON t.id = i.task_instance_id
WHERE t.session_id = ?
ORDER BY i.task_instance_id, i.position;

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
       j.judge_session, j.judge_workflow, j.relation, j.created_at
FROM task_done_when_judges j
JOIN task_instances t ON t.id = j.task_instance_id
WHERE t.session_id = ?;

-- Populations

-- name: GetPopulation :one
SELECT workflow, name FROM populations WHERE workflow = ? AND name = ?;

-- name: UpsertPopulation :exec
INSERT INTO populations (workflow, name) VALUES (?, ?)
ON CONFLICT(workflow, name) DO NOTHING;

-- name: ListPopulationMembers :many
SELECT workflow, name, resource_id, session_name, generation, accepted_at,
       last_appearance, last_inbound, tombstoned, pending_up,
       decision_kind, decision_reason, item_json
FROM population_members WHERE workflow = ? AND name = ? ORDER BY resource_id;

-- name: DeletePopulationMembersForPopulation :exec
DELETE FROM population_members WHERE workflow = ? AND name = ?;

-- name: InsertPopulationMember :exec
INSERT INTO population_members (
    workflow, name, resource_id, session_name, generation, accepted_at,
    last_appearance, last_inbound, tombstoned, pending_up,
    decision_kind, decision_reason, item_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: InsertPopulationMemberBlocker :exec
INSERT INTO population_member_blockers (workflow, name, resource_id, position, reason)
VALUES (?, ?, ?, ?, ?);

-- name: ListPopulationMemberBlockersForPopulation :many
SELECT b.workflow, b.name, b.resource_id, b.position, b.reason
FROM population_member_blockers b
WHERE b.workflow = ? AND b.name = ?
ORDER BY b.resource_id, b.position;

-- name: UpsertSessionChannelHealth :exec
INSERT INTO session_channel_health (
    session_id, kind, consecutive_failures, first_failure_at,
    last_failure_at, last_channel, last_error, escalated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, kind) DO UPDATE SET
    consecutive_failures = excluded.consecutive_failures,
    first_failure_at = excluded.first_failure_at,
    last_failure_at = excluded.last_failure_at,
    last_channel = excluded.last_channel,
    last_error = excluded.last_error,
    escalated_at = excluded.escalated_at;

-- name: DeleteSessionChannelHealth :exec
DELETE FROM session_channel_health WHERE session_id = ? AND kind = ?;

-- name: ListSessionChannelHealth :many
SELECT session_id, kind, consecutive_failures, first_failure_at,
       last_failure_at, last_channel, last_error, escalated_at
FROM session_channel_health WHERE session_id = ?;

-- Up-slot reservations

-- name: ListUpReservations :many
SELECT child_session_name, parent_session_name, virtual_root, pid, reserved_at
FROM up_reservations ORDER BY child_session_name;

-- name: UpsertUpReservation :exec
INSERT INTO up_reservations (child_session_name, parent_session_name, virtual_root, pid, reserved_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(child_session_name) DO UPDATE SET
    parent_session_name = excluded.parent_session_name,
    virtual_root = excluded.virtual_root,
    pid = excluded.pid,
    reserved_at = excluded.reserved_at;

-- name: DeleteUpReservation :exec
DELETE FROM up_reservations WHERE child_session_name = ?;

-- Events

-- name: NextEventSequence :one
SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE session_id = ?;

-- name: InsertEvent :exec
INSERT INTO events (id, session_id, sequence, time, type, source, direction, summary, body, metadata_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: DeleteEventsForSession :exec
DELETE FROM events WHERE session_id = ?;

-- name: ListEventsFromBySession :many
SELECT id, sequence, time, type, source, direction, summary, body, metadata_json
FROM events WHERE session_id = ? AND sequence >= ? ORDER BY sequence;

-- LatestEventByType backs the status-message reader.
-- name: LatestEventByType :one
SELECT id, sequence, time, type, source, direction, summary, body, metadata_json
FROM events WHERE session_id = ? AND type = ? ORDER BY sequence DESC LIMIT 1;

-- name: HasEventCursor :one
SELECT COUNT(*) FROM event_cursors WHERE session_id = ? AND kind = ?;

-- name: GetEventCursor :one
SELECT next_sequence FROM event_cursors WHERE session_id = ? AND kind = ?;

-- name: UpsertEventCursor :exec
INSERT INTO event_cursors (session_id, kind, next_sequence) VALUES (?, ?, ?)
ON CONFLICT(session_id, kind) DO UPDATE SET next_sequence = excluded.next_sequence;
