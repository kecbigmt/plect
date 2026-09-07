-- +goose Up
-- Atlas's own diff would create the new node_instances shape, drop
-- node_instance_layers, and create node_executions/node_execution_layers
-- with no data conversion between them -- silently losing every existing
-- node_instances/node_instance_layers row's setup-attempt data. This is
-- hand-written instead (see doc.go and docs/design/sqlite-persistence.md)
-- to carry that data forward as each node's first recorded execution before
-- the now-obsolete columns are dropped.
--
-- PRAGMA foreign_keys does not take effect inside an already-open
-- transaction, and goose always runs a migration's Up block in one, so the
-- statement below is documentation of intent, not something later
-- statements may rely on: the old "node_instances" is renamed out of the
-- way (never dropped) before node_executions' FK ever names the live
-- "node_instances" table, so no later DROP TABLE can cascade-delete rows
-- through a foreign key SQLite still enforces.
PRAGMA foreign_keys = off;
-- free the "node_instances" name for its final, identity-only shape
ALTER TABLE `node_instances` RENAME TO `old_node_instances`;
-- create "node_instances" in its final shape (the logical node's identity only)
CREATE TABLE `node_instances` (`session_id` text NOT NULL, `node_id` text NOT NULL, PRIMARY KEY (`session_id`, `node_id`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE);
-- copy logical node identities from "old_node_instances" into it
INSERT INTO `node_instances` (`session_id`, `node_id`) SELECT `session_id`, `node_id` FROM `old_node_instances`;
-- create "node_executions", referencing the final "node_instances" from the start
CREATE TABLE `node_executions` (`id` text NULL, `session_id` text NOT NULL, `node_id` text NOT NULL, `sequence` integer NOT NULL, `task_id` text NULL, `name` text NULL, `scope` text NOT NULL, `status` text NOT NULL, `resource` text NULL, `execution_dir` text NULL, `inputs_json` text NULL, `outputs_json` text NULL, `state_json` text NULL, `resource_observation_json` text NULL, `resource_observed_at` text NULL, `done_when_json` text NULL, `extra_done_when_json` text NULL, `cleanup_json` text NULL, `plugin_ref` text NULL, `error` text NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `finalized_at` text NULL, PRIMARY KEY (`id`), CONSTRAINT `0` FOREIGN KEY (`session_id`, `node_id`) REFERENCES `node_instances` (`session_id`, `node_id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (scope IN ('session', 'run')), CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (state_json IS NULL OR json_valid(state_json)), CHECK (resource_observation_json IS NULL OR json_valid(resource_observation_json)), CHECK (done_when_json IS NULL OR json_valid(done_when_json)), CHECK (extra_done_when_json IS NULL OR json_valid(extra_done_when_json)), CHECK (cleanup_json IS NULL OR json_valid(cleanup_json)));
-- carry every existing "old_node_instances" row forward as that node's first recorded execution
INSERT INTO `node_executions` (`id`, `session_id`, `node_id`, `sequence`, `task_id`, `name`, `scope`, `status`, `resource`, `execution_dir`, `inputs_json`, `outputs_json`, `state_json`, `resource_observation_json`, `resource_observed_at`, `done_when_json`, `extra_done_when_json`, `cleanup_json`, `plugin_ref`, `error`, `setup_at`, `failed_at`, `cleaned_at`, `finalized_at`)
SELECT lower(hex(randomblob(16))), `session_id`, `node_id`, `sequence`, `task_id`, `name`, `scope`, `status`, `resource`, NULL, `inputs_json`, `outputs_json`, `state_json`, `resource_observation_json`, `resource_observed_at`, `done_when_json`, `extra_done_when_json`, NULL, NULL, `error`, `setup_at`, `failed_at`, `cleaned_at`, `finalized_at`
FROM `old_node_instances`;
-- create "node_execution_layers" while old "node_instance_layers" still exists to read from
CREATE TABLE `node_execution_layers` (`execution_id` text NOT NULL, `position` integer NOT NULL, `effect_id` text NOT NULL, `status` text NOT NULL, `inputs_json` text NULL, `locals_json` text NULL, `outputs_json` text NULL, `env_json` text NULL, `heartbeat_ticks` integer NULL, `heartbeat_escalations` integer NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `error` text NULL, `cleanup_json` text NULL, PRIMARY KEY (`execution_id`, `position`), CONSTRAINT `0` FOREIGN KEY (`execution_id`) REFERENCES `node_executions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (locals_json IS NULL OR json_valid(locals_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (env_json IS NULL OR json_valid(env_json)), CHECK (cleanup_json IS NULL OR json_valid(cleanup_json)));
-- carry every existing node_instance_layers row forward, joined back to the execution just minted for its (session_id, node_id)
INSERT INTO `node_execution_layers` (`execution_id`, `position`, `effect_id`, `status`, `inputs_json`, `locals_json`, `outputs_json`, `env_json`, `heartbeat_ticks`, `heartbeat_escalations`, `setup_at`, `failed_at`, `cleaned_at`, `error`, `cleanup_json`)
SELECT `ne`.`id`, `nil`.`position`, `nil`.`effect_id`, `nil`.`status`, `nil`.`inputs_json`, `nil`.`locals_json`, `nil`.`outputs_json`, `nil`.`env_json`, `nil`.`heartbeat_ticks`, `nil`.`heartbeat_escalations`, `nil`.`setup_at`, `nil`.`failed_at`, `nil`.`cleaned_at`, `nil`.`error`, NULL
FROM `node_instance_layers` AS `nil`
JOIN `node_executions` AS `ne` ON `ne`.`session_id` = `nil`.`session_id` AND `ne`.`node_id` = `nil`.`node_id`;
-- drop "node_instance_layers": nothing's FK names it, so this cannot cascade
DROP TABLE `node_instance_layers`;
-- drop "old_node_instances": node_executions' FK already names the final
-- "node_instances" table instead, so this cannot cascade either
DROP TABLE `old_node_instances`;
-- create index "node_executions_session_node_idx" to table: "node_executions"
CREATE INDEX `node_executions_session_node_idx` ON `node_executions` (`session_id`, `node_id`, `sequence`);
-- create index "node_executions_one_unreleased_idx" to table: "node_executions"
CREATE UNIQUE INDEX `node_executions_one_unreleased_idx` ON `node_executions` (`session_id`, `node_id`) WHERE status <> 'cleaned';
-- create "node_execution_dependencies" table
CREATE TABLE `node_execution_dependencies` (`execution_id` text NOT NULL, `depends_on_execution_id` text NOT NULL, PRIMARY KEY (`execution_id`, `depends_on_execution_id`), CONSTRAINT `0` FOREIGN KEY (`depends_on_execution_id`) REFERENCES `node_executions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CONSTRAINT `1` FOREIGN KEY (`execution_id`) REFERENCES `node_executions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (execution_id <> depends_on_execution_id));
-- create index "node_execution_dependencies_depends_on_idx" to table: "node_execution_dependencies"
CREATE INDEX `node_execution_dependencies_depends_on_idx` ON `node_execution_dependencies` (`depends_on_execution_id`);
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- +goose Down
-- Best-effort, not lossless (matching the record_json-dissolution
-- migration's own Down above): a node with more than one recorded
-- execution collapses onto its latest one, and every
-- node_execution_dependencies edge is discarded, since the pre-this-change
-- schema had no column or table to hold either.
PRAGMA foreign_keys = off;
-- recreate "node_instances" (full pre-change columns) under a temporary name
CREATE TABLE `old_node_instances` (`session_id` text NOT NULL, `node_id` text NOT NULL, `task_id` text NULL, `name` text NULL, `scope` text NOT NULL, `status` text NOT NULL, `sequence` integer NOT NULL, `resource` text NULL, `inputs_json` text NULL, `outputs_json` text NULL, `state_json` text NULL, `resource_observation_json` text NULL, `resource_observed_at` text NULL, `done_when_json` text NULL, `extra_done_when_json` text NULL, `error` text NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `finalized_at` text NULL, PRIMARY KEY (`session_id`, `node_id`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (scope IN ('session', 'run')), CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (state_json IS NULL OR json_valid(state_json)), CHECK (resource_observation_json IS NULL OR json_valid(resource_observation_json)), CHECK (done_when_json IS NULL OR json_valid(done_when_json)), CHECK (extra_done_when_json IS NULL OR json_valid(extra_done_when_json)));
-- collapse each node onto its latest recorded execution
INSERT INTO `old_node_instances` (`session_id`, `node_id`, `task_id`, `name`, `scope`, `status`, `sequence`, `resource`, `inputs_json`, `outputs_json`, `state_json`, `resource_observation_json`, `resource_observed_at`, `done_when_json`, `extra_done_when_json`, `error`, `setup_at`, `failed_at`, `cleaned_at`, `finalized_at`)
SELECT `ne`.`session_id`, `ne`.`node_id`, `ne`.`task_id`, `ne`.`name`, `ne`.`scope`, `ne`.`status`, `ne`.`sequence`, `ne`.`resource`, `ne`.`inputs_json`, `ne`.`outputs_json`, `ne`.`state_json`, `ne`.`resource_observation_json`, `ne`.`resource_observed_at`, `ne`.`done_when_json`, `ne`.`extra_done_when_json`, `ne`.`error`, `ne`.`setup_at`, `ne`.`failed_at`, `ne`.`cleaned_at`, `ne`.`finalized_at`
FROM `node_executions` AS `ne`
JOIN (SELECT `node_id`, MAX(`sequence`) AS `max_sequence` FROM `node_executions` GROUP BY `node_id`) AS `latest`
  ON `latest`.`node_id` = `ne`.`node_id` AND `latest`.`max_sequence` = `ne`.`sequence`;
-- recreate "node_instance_layers" under its old name, from each collapsed node's latest execution's layers
CREATE TABLE `node_instance_layers` (`session_id` text NOT NULL, `node_id` text NOT NULL, `position` integer NOT NULL, `effect_id` text NOT NULL, `status` text NOT NULL, `inputs_json` text NULL, `locals_json` text NULL, `outputs_json` text NULL, `env_json` text NULL, `heartbeat_ticks` integer NULL, `heartbeat_escalations` integer NULL, `setup_at` text NULL, `failed_at` text NULL, `cleaned_at` text NULL, `error` text NULL, PRIMARY KEY (`session_id`, `node_id`, `position`), CONSTRAINT `0` FOREIGN KEY (`session_id`, `node_id`) REFERENCES `node_instances` (`session_id`, `node_id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (status IN ('produced', 'failed', 'cleaned')), CHECK (inputs_json IS NULL OR json_valid(inputs_json)), CHECK (locals_json IS NULL OR json_valid(locals_json)), CHECK (outputs_json IS NULL OR json_valid(outputs_json)), CHECK (env_json IS NULL OR json_valid(env_json)));
INSERT INTO `node_instance_layers` (`session_id`, `node_id`, `position`, `effect_id`, `status`, `inputs_json`, `locals_json`, `outputs_json`, `env_json`, `heartbeat_ticks`, `heartbeat_escalations`, `setup_at`, `failed_at`, `cleaned_at`, `error`)
SELECT `ne`.`session_id`, `ne`.`node_id`, `nel`.`position`, `nel`.`effect_id`, `nel`.`status`, `nel`.`inputs_json`, `nel`.`locals_json`, `nel`.`outputs_json`, `nel`.`env_json`, `nel`.`heartbeat_ticks`, `nel`.`heartbeat_escalations`, `nel`.`setup_at`, `nel`.`failed_at`, `nel`.`cleaned_at`, `nel`.`error`
FROM `node_execution_layers` AS `nel`
JOIN `node_executions` AS `ne` ON `ne`.`id` = `nel`.`execution_id`
JOIN (SELECT `node_id`, MAX(`sequence`) AS `max_sequence` FROM `node_executions` GROUP BY `node_id`) AS `latest`
  ON `latest`.`node_id` = `ne`.`node_id` AND `latest`.`max_sequence` = `ne`.`sequence`;
-- drop the execution-identity tables now that their latest generation is carried back
DROP TABLE `node_execution_dependencies`;
DROP TABLE `node_execution_layers`;
DROP TABLE `node_executions`;
DROP TABLE `node_instances`;
ALTER TABLE `old_node_instances` RENAME TO `node_instances`;
PRAGMA foreign_keys = on;
