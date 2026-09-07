-- +goose Up
-- Atlas's own generated diff for this rewrite naively copies old rows into
-- the new NOT NULL columns (id, status, session_id) with no value, which
-- fails outright against any database holding real rows -- this migration
-- is hand-written instead, following the same precedent as
-- drop_event_delivery_mode.sql. It reaches the identical structural end
-- state schema.sql declares (verified by TestSchemaSQL_MatchesMigrationHistory).
--
-- Backfill strategy for a database with pre-existing rows: the retired
-- event_streams table already minted one id per session incarnation, so
-- every migrated sessions row reuses its corresponding event_streams.id as
-- its own id -- a pre-existing session's events stay linked with no re-key.
-- The row matching each session's current name becomes that live row
-- (status 'down'; existing relational columns carried over); every other,
-- superseded event_streams row for a name becomes a minimal 'destroyed'
-- row (workflow unknown, so it takes the empty string the domain already
-- reserves for "no frozen workflow"; destroyed_at approximated as that
-- incarnation's last event time, falling back to its stream's created_at).
-- A session that never logged an event gets a freshly minted placeholder
-- id instead. Every *_json/error/timestamp column this migration adds to
-- node_instances/task_instances/task_done_when_states/population_members
-- is left NULL for pre-existing rows rather than parsed out of the retired
-- record_json blob -- see docs/migrations/ for why an in-place backfill of
-- that data is out of scope here.
PRAGMA foreign_keys = off;

ALTER TABLE `sessions` RENAME TO `old_sessions`;
-- The rename carries the old table's own indexes along under their
-- original names; drop them before the new sessions table below recreates
-- both under the same names.
DROP INDEX `sessions_alias_idx`;
DROP INDEX `sessions_parent_idx`;

CREATE TABLE `sessions` (
  `id` text NULL,
  `name` text NOT NULL,
  `status` text NOT NULL,
  `destroyed_at` text NULL,
  `parent_session_id` text NULL,
  `root_session_id` text NULL,
  `resource_id` text NULL,
  `alias` text NULL,
  `branch` text NULL,
  `workflow` text NOT NULL,
  `workspace_dir` text NULL,
  `population_workflow` text NULL,
  `population_name` text NULL,
  `inputs_json` text NULL,
  `health_last_checked_at` text NULL,
  `health_last_activity_at` text NULL,
  `health_last_fingerprint` text NULL,
  `health_last_state` text NULL,
  `health_last_reason` text NULL,
  `health_last_notified_at` text NULL,
  `health_notify_count` integer NULL,
  `tick_consecutive_unchanged` integer NULL,
  `tick_last_fingerprint` text NULL,
  `last_tick_at` text NULL,
  `created_at` text NOT NULL,
  `updated_at` text NOT NULL,
  PRIMARY KEY (`id`),
  CONSTRAINT `0` FOREIGN KEY (`population_workflow`, `population_name`) REFERENCES `populations` (`workflow`, `name`) ON UPDATE NO ACTION ON DELETE SET NULL,
  CONSTRAINT `1` FOREIGN KEY (`root_session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE NO ACTION,
  CONSTRAINT `2` FOREIGN KEY (`parent_session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE NO ACTION,
  CHECK (status IN ('down', 'up', 'destroyed')),
  CHECK (inputs_json IS NULL OR json_valid(inputs_json)),
  CHECK (health_last_state IS NULL OR health_last_state IN ('healthy', 'unhealthy', 'stalled', 'undeclared')),
  CHECK ((status = 'destroyed') = (destroyed_at IS NOT NULL)),
  CHECK (NOT (parent_session_id IS NOT NULL AND root_session_id IS NOT NULL)),
  CHECK (root_session_id IS NULL OR root_session_id <> id),
  CHECK ((population_workflow IS NULL) = (population_name IS NULL))
);

-- The live row per pre-existing session name.
INSERT INTO `sessions` (
  `id`, `name`, `status`, `resource_id`, `alias`, `workflow`, `workspace_dir`,
  `population_workflow`, `population_name`, `created_at`, `updated_at`
)
SELECT
  COALESCE(
    (SELECT es.id FROM `event_streams` es WHERE es.session_name = o.name ORDER BY es.created_at DESC LIMIT 1),
    lower(hex(randomblob(16)))
  ),
  o.name, 'down', o.resource_id, o.alias, o.workflow, o.workspace_dir,
  o.population_workflow, o.population_name, o.created_at, o.updated_at
FROM `old_sessions` o;

-- Backfill parent/root by resolving the recorded name against the id just minted for it.
UPDATE `sessions`
SET `parent_session_id` = (
  SELECT p.id FROM `sessions` p
  JOIN `old_sessions` o ON o.name = `sessions`.name
  WHERE p.name = o.parent_session_name
)
WHERE EXISTS (SELECT 1 FROM `old_sessions` o WHERE o.name = `sessions`.name AND o.parent_session_name IS NOT NULL);

UPDATE `sessions`
SET `root_session_id` = (
  SELECT p.id FROM `sessions` p
  JOIN `old_sessions` o ON o.name = `sessions`.name
  WHERE p.name = o.root_session_name
)
WHERE EXISTS (SELECT 1 FROM `old_sessions` o WHERE o.name = `sessions`.name AND o.root_session_name IS NOT NULL);

-- Every event_streams row not already reused above is a superseded
-- (already-destroyed under the old model) incarnation with no surviving
-- rich session row; reconstruct a minimal destroyed placeholder so its
-- events keep a valid session_id to reference.
INSERT INTO `sessions` (`id`, `name`, `status`, `destroyed_at`, `workflow`, `created_at`, `updated_at`)
SELECT
  es.id, es.session_name, 'destroyed',
  COALESCE((SELECT MAX(e.time) FROM `events` e WHERE e.stream_id = es.id), es.created_at),
  '', es.created_at,
  COALESCE((SELECT MAX(e.time) FROM `events` e WHERE e.stream_id = es.id), es.created_at)
FROM `event_streams` es
WHERE es.id NOT IN (SELECT id FROM `sessions`);

CREATE UNIQUE INDEX `sessions_live_name` ON `sessions` (`name`) WHERE status <> 'destroyed';
CREATE INDEX `sessions_alias_idx` ON `sessions` (`alias`);
CREATE INDEX `sessions_parent_idx` ON `sessions` (`parent_session_id`);

DROP TABLE `old_sessions`;

-- create "new_node_instances" table
CREATE TABLE `new_node_instances` (`session_id` text NOT NULL, `node_id` text NOT NULL, `task_id` text NULL, `name` text NULL, `scope` text NOT NULL, `status` text NOT NULL, `sequence` integer NOT NULL, `resource` text NULL, `inputs_json` text NULL, `outputs_json` text NULL, `state_json` text NULL, `resource_observation_json` text NULL, `resource_observed_at` text NULL, `done_when_json` text NULL, `extra_done_when_json` text NULL, `error` text NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `finalized_at` text NULL, PRIMARY KEY (`session_id`, `node_id`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (scope IN ('session', 'run')), CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (state_json IS NULL OR json_valid(state_json)), CHECK (resource_observation_json IS NULL OR json_valid(resource_observation_json)), CHECK (done_when_json IS NULL OR json_valid(done_when_json)), CHECK (extra_done_when_json IS NULL OR json_valid(extra_done_when_json)));
INSERT INTO `new_node_instances` (`session_id`, `node_id`, `scope`, `status`, `sequence`, `finalized_at`)
SELECT (SELECT id FROM `sessions` WHERE `sessions`.name = ni.session_name AND `sessions`.status <> 'destroyed'), ni.node_id, ni.scope, ni.status, ni.sequence, ni.finalized_at
FROM `node_instances` ni;
DROP TABLE `node_instances`;
ALTER TABLE `new_node_instances` RENAME TO `node_instances`;

-- create "new_task_instances" table
CREATE TABLE `new_task_instances` (`id` text NULL, `session_id` text NOT NULL, `instance_name` text NOT NULL, `task_id` text NOT NULL, `scope` text NOT NULL, `status` text NOT NULL, `sequence` integer NOT NULL, `resource` text NULL, `named` boolean NOT NULL, `inputs_json` text NULL, `outputs_json` text NULL, `state_json` text NULL, `resource_observation_json` text NULL, `resource_observed_at` text NULL, `extra_done_when_json` text NULL, `error` text NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `finalized_at` text NULL, PRIMARY KEY (`id`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (scope IN ('session', 'run')), CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (named IN (0, 1)), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (state_json IS NULL OR json_valid(state_json)), CHECK (resource_observation_json IS NULL OR json_valid(resource_observation_json)), CHECK (extra_done_when_json IS NULL OR json_valid(extra_done_when_json)));
INSERT INTO `new_task_instances` (`id`, `session_id`, `instance_name`, `task_id`, `scope`, `status`, `sequence`, `resource`, `named`, `finalized_at`)
SELECT ti.id, (SELECT id FROM `sessions` WHERE `sessions`.name = ti.session_name AND `sessions`.status <> 'destroyed'), ti.instance_name, ti.task_id, ti.scope, ti.status, ti.sequence, ti.resource, ti.named, ti.finalized_at
FROM `task_instances` ti;
DROP TABLE `task_instances`;
ALTER TABLE `new_task_instances` RENAME TO `task_instances`;
CREATE UNIQUE INDEX `task_instances_session_id_instance_name` ON `task_instances` (`session_id`, `instance_name`);

-- create "new_task_done_when_states" table (drops last_unsatisfied_json; see task_done_when_unsatisfied_items below)
CREATE TABLE `new_task_done_when_states` (`task_instance_id` text NULL, `heartbeat_ticks` integer NOT NULL DEFAULT 0, `heartbeat_escalations` integer NOT NULL DEFAULT 0, `last_action` text NULL, `last_fingerprint` text NULL, `last_reason` text NULL, `last_body` text NULL, `escalated_at` text NULL, `escalate_reason` text NULL, PRIMARY KEY (`task_instance_id`), CONSTRAINT `0` FOREIGN KEY (`task_instance_id`) REFERENCES `task_instances` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (last_action IN ('satisfied', 'wait', 'escalate', 'review_required', 'kick')));
INSERT INTO `new_task_done_when_states` (`task_instance_id`, `heartbeat_ticks`, `heartbeat_escalations`, `last_action`, `last_fingerprint`, `last_reason`, `last_body`, `escalated_at`, `escalate_reason`)
SELECT `task_instance_id`, `heartbeat_ticks`, `heartbeat_escalations`, `last_action`, `last_fingerprint`, `last_reason`, `last_body`, `escalated_at`, `escalate_reason` FROM `task_done_when_states`;
CREATE TABLE `task_done_when_unsatisfied_items` (`task_instance_id` text NOT NULL, `position` integer NOT NULL, `item` text NOT NULL, PRIMARY KEY (`task_instance_id`, `position`), CONSTRAINT `0` FOREIGN KEY (`task_instance_id`) REFERENCES `task_instances` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE);
INSERT INTO `task_done_when_unsatisfied_items` (`task_instance_id`, `position`, `item`)
SELECT s.task_instance_id, je.key, je.value
FROM `task_done_when_states` s, json_each(s.last_unsatisfied_json) je
WHERE json_valid(s.last_unsatisfied_json);
DROP TABLE `task_done_when_states`;
ALTER TABLE `new_task_done_when_states` RENAME TO `task_done_when_states`;

-- create "new_population_members" table (drops last_blockers_json; see population_member_blockers below)
CREATE TABLE `new_population_members` (`workflow` text NOT NULL, `name` text NOT NULL, `resource_id` text NOT NULL, `session_name` text NULL, `generation` integer NOT NULL DEFAULT 0, `accepted_at` text NULL, `last_appearance` text NULL, `last_inbound` text NULL, `tombstoned` boolean NOT NULL DEFAULT 0, `pending_up` boolean NOT NULL DEFAULT 0, `decision_kind` text NULL, `decision_reason` text NULL, `item_json` text NOT NULL DEFAULT '{}', PRIMARY KEY (`workflow`, `name`, `resource_id`), CONSTRAINT `0` FOREIGN KEY (`workflow`, `name`) REFERENCES `populations` (`workflow`, `name`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (tombstoned IN (0, 1)), CHECK (pending_up IN (0, 1)), CHECK (decision_kind IN ('plect.workflow_population.destroy', 'plect.workflow_population.destroy_deferred', 'plect.workflow_population.destroy_dry_run')), CHECK (json_valid(item_json)));
INSERT INTO `new_population_members` (`workflow`, `name`, `resource_id`, `session_name`, `generation`, `accepted_at`, `last_appearance`, `last_inbound`, `tombstoned`, `pending_up`, `decision_kind`, `decision_reason`, `item_json`)
SELECT `workflow`, `name`, `resource_id`, `session_name`, `generation`, `accepted_at`, `last_appearance`, `last_inbound`, `tombstoned`, `pending_up`, `decision_kind`, `decision_reason`, `item_json` FROM `population_members`;
CREATE TABLE `population_member_blockers` (`workflow` text NOT NULL, `name` text NOT NULL, `resource_id` text NOT NULL, `position` integer NOT NULL, `reason` text NOT NULL, PRIMARY KEY (`workflow`, `name`, `resource_id`, `position`), CONSTRAINT `0` FOREIGN KEY (`workflow`, `name`, `resource_id`) REFERENCES `population_members` (`workflow`, `name`, `resource_id`) ON UPDATE NO ACTION ON DELETE CASCADE);
INSERT INTO `population_member_blockers` (`workflow`, `name`, `resource_id`, `position`, `reason`)
SELECT m.workflow, m.name, m.resource_id, je.key, je.value
FROM `population_members` m, json_each(m.last_blockers_json) je
WHERE json_valid(m.last_blockers_json);
DROP TABLE `population_members`;
ALTER TABLE `new_population_members` RENAME TO `population_members`;

CREATE TABLE `node_instance_layers` (`session_id` text NOT NULL, `node_id` text NOT NULL, `position` integer NOT NULL, `effect_id` text NOT NULL, `status` text NOT NULL, `inputs_json` text NULL, `locals_json` text NULL, `outputs_json` text NULL, `env_json` text NULL, `heartbeat_ticks` integer NULL, `heartbeat_escalations` integer NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `error` text NULL, PRIMARY KEY (`session_id`, `node_id`, `position`), CONSTRAINT `0` FOREIGN KEY (`session_id`, `node_id`) REFERENCES `node_instances` (`session_id`, `node_id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (locals_json IS NULL OR json_valid(locals_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (env_json IS NULL OR json_valid(env_json)));
CREATE TABLE `task_instance_layers` (`task_instance_id` text NOT NULL, `position` integer NOT NULL, `effect_id` text NOT NULL, `status` text NOT NULL, `inputs_json` text NULL, `locals_json` text NULL, `outputs_json` text NULL, `env_json` text NULL, `heartbeat_ticks` integer NULL, `heartbeat_escalations` integer NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `error` text NULL, PRIMARY KEY (`task_instance_id`, `position`), CONSTRAINT `0` FOREIGN KEY (`task_instance_id`) REFERENCES `task_instances` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (locals_json IS NULL OR json_valid(locals_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (env_json IS NULL OR json_valid(env_json)));
CREATE TABLE `session_channel_health` (`session_id` text NOT NULL, `kind` text NOT NULL, `consecutive_failures` integer NOT NULL, `first_failure_at` text NOT NULL, `last_failure_at` text NOT NULL, `last_channel` text NULL, `last_error` text NULL, `escalated_at` text NULL, PRIMARY KEY (`session_id`, `kind`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (kind IN ('validation', 'delivery')));

-- create "new_events" table
CREATE TABLE `new_events` (`id` text NULL, `session_id` text NOT NULL, `sequence` integer NOT NULL, `time` text NOT NULL, `type` text NOT NULL, `source` text NOT NULL, `direction` text NOT NULL, `summary` text NOT NULL, `body` text NOT NULL DEFAULT '', `metadata_json` text NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE NO ACTION, CHECK (sequence > 0), CHECK (direction IN ('inbound', 'outbound', 'internal')), CHECK (json_valid(metadata_json)));
INSERT INTO `new_events` (`id`, `session_id`, `sequence`, `time`, `type`, `source`, `direction`, `summary`, `body`, `metadata_json`)
SELECT `id`, `stream_id`, `sequence`, `time`, `type`, `source`, `direction`, `summary`, `body`, `metadata_json` FROM `events`;
DROP TABLE `events`;
ALTER TABLE `new_events` RENAME TO `events`;
CREATE UNIQUE INDEX `events_session_id_sequence` ON `events` (`session_id`, `sequence`);
CREATE INDEX `events_session_id_id_idx` ON `events` (`session_id`, `id`);
CREATE INDEX `events_session_id_type_sequence_idx` ON `events` (`session_id`, `type`, `sequence`);

-- create "new_event_cursors" table
CREATE TABLE `new_event_cursors` (`session_id` text NOT NULL, `kind` text NOT NULL, `next_sequence` integer NOT NULL, PRIMARY KEY (`session_id`, `kind`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (kind IN ('delivery', 'tick', 'heartbeat')), CHECK (next_sequence >= 0));
INSERT INTO `new_event_cursors` (`session_id`, `kind`, `next_sequence`)
SELECT `stream_id`, `kind`, `next_sequence` FROM `event_cursors`;
DROP TABLE `event_cursors`;
ALTER TABLE `new_event_cursors` RENAME TO `event_cursors`;

DROP TABLE `event_streams`;

PRAGMA foreign_keys = on;

-- +goose Down
-- Best-effort structural rollback: a destroyed session row (impossible in
-- the old one-row-per-name schema) is dropped rather than preserved, and
-- record_json/last_unsatisfied_json/last_blockers_json are reconstructed
-- empty rather than repopulated from the columns this migration retires
-- them into -- restoring data losslessly across a forward-incompatible
-- schema flip is not the goal; not erroring on the way back down is.
PRAGMA foreign_keys = off;

CREATE TABLE `event_streams` (`id` text NULL, `session_name` text NOT NULL, `created_at` text NOT NULL, PRIMARY KEY (`id`));
INSERT INTO `event_streams` (`id`, `session_name`, `created_at`) SELECT `id`, `name`, `created_at` FROM `sessions`;
CREATE INDEX `event_streams_session_name_created_at` ON `event_streams` (`session_name`, `created_at` DESC);

CREATE TABLE `old_events` (`id` text NULL, `stream_id` text NOT NULL, `sequence` integer NOT NULL, `time` text NOT NULL, `type` text NOT NULL, `source` text NOT NULL, `direction` text NOT NULL, `summary` text NOT NULL, `body` text NOT NULL DEFAULT '', `metadata_json` text NOT NULL, PRIMARY KEY (`id`));
INSERT INTO `old_events` SELECT `id`, `session_id`, `sequence`, `time`, `type`, `source`, `direction`, `summary`, `body`, `metadata_json` FROM `events`;
DROP TABLE `events`;
ALTER TABLE `old_events` RENAME TO `events`;
CREATE UNIQUE INDEX `events_stream_id_sequence` ON `events` (`stream_id`, `sequence`);
CREATE INDEX `events_stream_id_id_idx` ON `events` (`stream_id`, `id`);

CREATE TABLE `old_event_cursors` (`stream_id` text NOT NULL, `kind` text NOT NULL, `next_sequence` integer NOT NULL, PRIMARY KEY (`stream_id`, `kind`));
INSERT INTO `old_event_cursors` SELECT `session_id`, `kind`, `next_sequence` FROM `event_cursors`;
DROP TABLE `event_cursors`;
ALTER TABLE `old_event_cursors` RENAME TO `event_cursors`;

DROP TABLE `session_channel_health`;
DROP TABLE `task_instance_layers`;
DROP TABLE `node_instance_layers`;

CREATE TABLE `old_population_members` (`workflow` text NOT NULL, `name` text NOT NULL, `resource_id` text NOT NULL, `session_name` text NULL, `generation` integer NOT NULL DEFAULT 0, `accepted_at` text NULL, `last_appearance` text NULL, `last_inbound` text NULL, `tombstoned` boolean NOT NULL DEFAULT 0, `pending_up` boolean NOT NULL DEFAULT 0, `decision_kind` text NULL, `decision_reason` text NULL, `item_json` text NOT NULL DEFAULT '{}', `last_blockers_json` text NOT NULL DEFAULT '[]', PRIMARY KEY (`workflow`, `name`, `resource_id`));
INSERT INTO `old_population_members` (`workflow`, `name`, `resource_id`, `session_name`, `generation`, `accepted_at`, `last_appearance`, `last_inbound`, `tombstoned`, `pending_up`, `decision_kind`, `decision_reason`, `item_json`)
SELECT `workflow`, `name`, `resource_id`, `session_name`, `generation`, `accepted_at`, `last_appearance`, `last_inbound`, `tombstoned`, `pending_up`, `decision_kind`, `decision_reason`, `item_json` FROM `population_members`;
DROP TABLE `population_member_blockers`;
DROP TABLE `population_members`;
ALTER TABLE `old_population_members` RENAME TO `population_members`;

CREATE TABLE `old_task_done_when_states` (`task_instance_id` text PRIMARY KEY, `heartbeat_ticks` integer NOT NULL DEFAULT 0, `heartbeat_escalations` integer NOT NULL DEFAULT 0, `last_action` text NULL, `last_fingerprint` text NULL, `last_reason` text NULL, `last_unsatisfied_json` text NOT NULL DEFAULT '[]', `last_body` text NULL, `escalated_at` text NULL, `escalate_reason` text NULL);
INSERT INTO `old_task_done_when_states` (`task_instance_id`, `heartbeat_ticks`, `heartbeat_escalations`, `last_action`, `last_fingerprint`, `last_reason`, `last_body`, `escalated_at`, `escalate_reason`)
SELECT `task_instance_id`, `heartbeat_ticks`, `heartbeat_escalations`, `last_action`, `last_fingerprint`, `last_reason`, `last_body`, `escalated_at`, `escalate_reason` FROM `task_done_when_states`;
DROP TABLE `task_done_when_unsatisfied_items`;
DROP TABLE `task_done_when_states`;
ALTER TABLE `old_task_done_when_states` RENAME TO `task_done_when_states`;

CREATE TABLE `old_task_instances` (`id` text PRIMARY KEY, `session_name` text NOT NULL, `instance_name` text NOT NULL, `task_id` text NOT NULL, `scope` text NOT NULL, `status` text NOT NULL, `sequence` integer NOT NULL, `resource` text NULL, `named` boolean NOT NULL, `finalized_at` text NULL, `record_json` text NOT NULL DEFAULT '{}');
INSERT INTO `old_task_instances` (`id`, `session_name`, `instance_name`, `task_id`, `scope`, `status`, `sequence`, `resource`, `named`, `finalized_at`)
SELECT ti.id, (SELECT name FROM `sessions` WHERE `sessions`.id = ti.session_id), ti.instance_name, ti.task_id, ti.scope, ti.status, ti.sequence, ti.resource, ti.named, ti.finalized_at
FROM `task_instances` ti;
DROP TABLE `task_instances`;
ALTER TABLE `old_task_instances` RENAME TO `task_instances`;
CREATE UNIQUE INDEX `task_instances_session_name_instance_name` ON `task_instances` (`session_name`, `instance_name`);

CREATE TABLE `old_node_instances` (`session_name` text NOT NULL, `node_id` text NOT NULL, `scope` text NOT NULL, `status` text NOT NULL, `sequence` integer NOT NULL, `finalized_at` text NULL, `record_json` text NOT NULL DEFAULT '{}', PRIMARY KEY (`session_name`, `node_id`));
INSERT INTO `old_node_instances` (`session_name`, `node_id`, `scope`, `status`, `sequence`, `finalized_at`)
SELECT (SELECT name FROM `sessions` WHERE `sessions`.id = ni.session_id), ni.node_id, ni.scope, ni.status, ni.sequence, ni.finalized_at
FROM `node_instances` ni;
DROP TABLE `node_instances`;
ALTER TABLE `old_node_instances` RENAME TO `node_instances`;

CREATE TABLE `old_sessions` (`name` text PRIMARY KEY, `parent_session_name` text NULL, `root_session_name` text NULL, `resource_id` text NULL, `alias` text NULL, `workflow` text NOT NULL, `workspace_dir` text NULL, `population_workflow` text NULL, `population_name` text NULL, `created_at` text NOT NULL, `updated_at` text NOT NULL, `record_json` text NOT NULL DEFAULT '{}');
INSERT INTO `old_sessions` (`name`, `parent_session_name`, `root_session_name`, `resource_id`, `alias`, `workflow`, `workspace_dir`, `population_workflow`, `population_name`, `created_at`, `updated_at`)
SELECT s.name,
  (SELECT p.name FROM `sessions` p WHERE p.id = s.parent_session_id),
  (SELECT p.name FROM `sessions` p WHERE p.id = s.root_session_id),
  s.resource_id, s.alias, s.workflow, s.workspace_dir, s.population_workflow, s.population_name, s.created_at, s.updated_at
FROM `sessions` s
WHERE s.status <> 'destroyed';
DROP TABLE `sessions`;
ALTER TABLE `old_sessions` RENAME TO `sessions`;
CREATE INDEX `sessions_alias_idx` ON `sessions` (`alias`);
CREATE INDEX `sessions_parent_idx` ON `sessions` (`parent_session_name`);

PRAGMA foreign_keys = on;
