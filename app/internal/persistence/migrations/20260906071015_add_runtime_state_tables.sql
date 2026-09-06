-- +goose Up
-- create "sessions" table
CREATE TABLE `sessions` (`name` text NULL, `parent_session_name` text NULL, `root_session_name` text NULL, `resource_id` text NOT NULL DEFAULT '', `alias` text NOT NULL DEFAULT '', `workflow` text NOT NULL DEFAULT '', `workspace_dir_path` text NOT NULL DEFAULT '', `created_at` text NOT NULL, `updated_at` text NOT NULL, `record_json` text NOT NULL, PRIMARY KEY (`name`), CONSTRAINT `0` FOREIGN KEY (`root_session_name`) REFERENCES `sessions` (`name`) ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT `1` FOREIGN KEY (`parent_session_name`) REFERENCES `sessions` (`name`) ON UPDATE NO ACTION ON DELETE SET NULL, CHECK (NOT (parent_session_name IS NOT NULL AND root_session_name IS NOT NULL)), CHECK (root_session_name IS NULL OR root_session_name <> name));
-- create index "sessions_alias_idx" to table: "sessions"
CREATE INDEX `sessions_alias_idx` ON `sessions` (`alias`) WHERE alias <> '';
-- create index "sessions_parent_idx" to table: "sessions"
CREATE INDEX `sessions_parent_idx` ON `sessions` (`parent_session_name`);
-- create "node_instances" table
CREATE TABLE `node_instances` (`session_name` text NOT NULL, `node_id` text NOT NULL, `scope` text NOT NULL, `status` text NOT NULL, `sequence` integer NOT NULL DEFAULT 0, `record_json` text NOT NULL, PRIMARY KEY (`session_name`, `node_id`), CONSTRAINT `0` FOREIGN KEY (`session_name`) REFERENCES `sessions` (`name`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (scope IN ('session', 'run')), CHECK (status IN ('produced', 'failed', 'cleaned')));
-- create "task_instances" table
CREATE TABLE `task_instances` (`id` text NULL, `session_name` text NOT NULL, `instance_name` text NOT NULL, `task_id` text NOT NULL DEFAULT '', `scope` text NOT NULL, `status` text NOT NULL, `sequence` integer NOT NULL DEFAULT 0, `resource` text NOT NULL DEFAULT '', `named_instance` text NOT NULL DEFAULT '', `record_json` text NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `0` FOREIGN KEY (`session_name`) REFERENCES `sessions` (`name`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (scope IN ('session', 'run')), CHECK (status IN ('produced', 'failed', 'cleaned')));
-- create index "task_instances_session_name_instance_name" to table: "task_instances"
CREATE UNIQUE INDEX `task_instances_session_name_instance_name` ON `task_instances` (`session_name`, `instance_name`);
-- create "task_done_when_states" table
CREATE TABLE `task_done_when_states` (`task_instance_id` text NULL, `heartbeat_ticks` integer NOT NULL DEFAULT 0, `heartbeat_escalations` integer NOT NULL DEFAULT 0, `last_action` text NOT NULL DEFAULT '', `last_fingerprint` text NOT NULL DEFAULT '', `last_reason` text NOT NULL DEFAULT '', `last_unsatisfied_json` text NOT NULL DEFAULT '[]', `last_body` text NOT NULL DEFAULT '', `escalated_at` text NOT NULL DEFAULT '', `escalate_reason` text NOT NULL DEFAULT '', PRIMARY KEY (`task_instance_id`), CONSTRAINT `0` FOREIGN KEY (`task_instance_id`) REFERENCES `task_instances` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE);
-- create "task_done_when_judges" table
CREATE TABLE `task_done_when_judges` (`task_instance_id` text NOT NULL, `leaf_id` text NOT NULL, `action` text NOT NULL, `reason` text NOT NULL DEFAULT '', `revision` text NOT NULL DEFAULT '', `judge_session` text NOT NULL DEFAULT '', `judge_workflow` text NOT NULL DEFAULT '', `relation` text NOT NULL DEFAULT '', `created_at` text NOT NULL, PRIMARY KEY (`task_instance_id`, `leaf_id`), CONSTRAINT `0` FOREIGN KEY (`task_instance_id`) REFERENCES `task_instances` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (action IN ('approve', 'request_changes')), CHECK (relation IN ('', 'self', 'parent', 'child', 'sibling', 'ancestor', 'descendant', 'unrelated')));
-- create "populations" table
CREATE TABLE `populations` (`population_key` text NULL, `workflow` text NOT NULL DEFAULT '', `name` text NOT NULL DEFAULT '', PRIMARY KEY (`population_key`));
-- create "population_members" table
CREATE TABLE `population_members` (`population_key` text NOT NULL, `resource_id` text NOT NULL, `session_name` text NOT NULL DEFAULT '', `generation` integer NOT NULL DEFAULT 0, `accepted_at` text NOT NULL DEFAULT '', `last_appearance` text NOT NULL DEFAULT '', `last_inbound` text NOT NULL DEFAULT '', `tombstoned` boolean NOT NULL DEFAULT 0, `pending_up` boolean NOT NULL DEFAULT 0, `last_decision` text NOT NULL DEFAULT '', `item_json` text NOT NULL DEFAULT '{}', `last_blockers_json` text NOT NULL DEFAULT '[]', PRIMARY KEY (`population_key`, `resource_id`), CONSTRAINT `0` FOREIGN KEY (`population_key`) REFERENCES `populations` (`population_key`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (tombstoned IN (0, 1)), CHECK (pending_up IN (0, 1)));
-- create "up_reservations" table
CREATE TABLE `up_reservations` (`child_session_name` text NULL, `parent_name` text NOT NULL DEFAULT '', `pid` integer NOT NULL, `reserved_at` text NOT NULL, PRIMARY KEY (`child_session_name`));

-- +goose Down
-- reverse: create "up_reservations" table
DROP TABLE `up_reservations`;
-- reverse: create "population_members" table
DROP TABLE `population_members`;
-- reverse: create "populations" table
DROP TABLE `populations`;
-- reverse: create "task_done_when_judges" table
DROP TABLE `task_done_when_judges`;
-- reverse: create "task_done_when_states" table
DROP TABLE `task_done_when_states`;
-- reverse: create index "task_instances_session_name_instance_name" to table: "task_instances"
DROP INDEX `task_instances_session_name_instance_name`;
-- reverse: create "task_instances" table
DROP TABLE `task_instances`;
-- reverse: create "node_instances" table
DROP TABLE `node_instances`;
-- reverse: create index "sessions_parent_idx" to table: "sessions"
DROP INDEX `sessions_parent_idx`;
-- reverse: create index "sessions_alias_idx" to table: "sessions"
DROP INDEX `sessions_alias_idx`;
-- reverse: create "sessions" table
DROP TABLE `sessions`;
