-- +goose Up
-- disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- create "new_events" table
CREATE TABLE `new_events` (`id` text NULL, `stream_id` text NOT NULL, `sequence` integer NOT NULL, `time` text NOT NULL, `type` text NOT NULL, `source` text NOT NULL, `direction` text NOT NULL, `summary` text NOT NULL, `body` text NOT NULL DEFAULT '', `metadata_json` text NOT NULL, PRIMARY KEY (`id`), CONSTRAINT `0` FOREIGN KEY (`stream_id`) REFERENCES `event_streams` (`id`) ON UPDATE NO ACTION ON DELETE NO ACTION, CHECK (sequence > 0), CHECK (direction IN ('inbound', 'outbound', 'internal')), CHECK (json_valid(metadata_json)));
-- copy rows from old table "events" to new temporary table "new_events"
INSERT INTO `new_events` (`id`, `stream_id`, `sequence`, `time`, `type`, `source`, `direction`, `summary`, `body`, `metadata_json`) SELECT `id`, `stream_id`, `sequence`, `time`, `type`, `source`, `direction`, `summary`, `body`, `metadata_json` FROM `events`;
-- drop "events" table after copying rows
DROP TABLE `events`;
-- rename temporary table "new_events" to "events"
ALTER TABLE `new_events` RENAME TO `events`;
-- create index "events_stream_id_sequence" to table: "events"
CREATE UNIQUE INDEX `events_stream_id_sequence` ON `events` (`stream_id`, `sequence`);
-- create index "events_stream_id_id_idx" to table: "events"
CREATE INDEX `events_stream_id_id_idx` ON `events` (`stream_id`, `id`);
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- +goose Down
-- reverse: create index "events_stream_id_id_idx" to table: "events"
DROP INDEX `events_stream_id_id_idx`;
-- reverse: create index "events_stream_id_sequence" to table: "events"
DROP INDEX `events_stream_id_sequence`;
-- reverse: create "new_events" table
DROP TABLE `new_events`;
