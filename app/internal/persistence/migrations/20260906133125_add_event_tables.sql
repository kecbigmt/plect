-- +goose Up
-- create "event_streams" table
CREATE TABLE `event_streams` (`id` text NULL, `session_name` text NOT NULL, PRIMARY KEY (`id`));
-- create index "event_streams_session_name" to table: "event_streams"
CREATE UNIQUE INDEX `event_streams_session_name` ON `event_streams` (`session_name`);
-- create "events" table
CREATE TABLE `events` (`event_id` text NULL, `stream_id` text NOT NULL, `sequence` integer NOT NULL, `recorded_at` text NOT NULL, `type` text NOT NULL, `source` text NOT NULL, `direction` text NOT NULL, `summary` text NOT NULL, `body` text NOT NULL DEFAULT '', `metadata_json` text NOT NULL, PRIMARY KEY (`event_id`), CONSTRAINT `0` FOREIGN KEY (`stream_id`) REFERENCES `event_streams` (`id`) ON UPDATE NO ACTION ON DELETE NO ACTION, CHECK (sequence > 0), CHECK (direction IN ('inbound', 'outbound', 'internal')));
-- create index "events_stream_id_sequence" to table: "events"
CREATE UNIQUE INDEX `events_stream_id_sequence` ON `events` (`stream_id`, `sequence`);
-- create index "events_stream_id_event_id_idx" to table: "events"
CREATE INDEX `events_stream_id_event_id_idx` ON `events` (`stream_id`, `event_id`);
-- create "event_cursors" table
CREATE TABLE `event_cursors` (`stream_id` text NOT NULL, `cursor_name` text NOT NULL, `next_sequence` integer NOT NULL, PRIMARY KEY (`stream_id`, `cursor_name`), CONSTRAINT `0` FOREIGN KEY (`stream_id`) REFERENCES `event_streams` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (cursor_name IN ('dispatcher', 'reactor', 'heartbeat_inbound')), CHECK (next_sequence >= 0));

-- +goose Down
-- reverse: create "event_cursors" table
DROP TABLE `event_cursors`;
-- reverse: create index "events_stream_id_event_id_idx" to table: "events"
DROP INDEX `events_stream_id_event_id_idx`;
-- reverse: create index "events_stream_id_sequence" to table: "events"
DROP INDEX `events_stream_id_sequence`;
-- reverse: create "events" table
DROP TABLE `events`;
-- reverse: create index "event_streams_session_name" to table: "event_streams"
DROP INDEX `event_streams_session_name`;
-- reverse: create "event_streams" table
DROP TABLE `event_streams`;
