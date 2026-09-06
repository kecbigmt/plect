-- +goose Up
-- create "event_streams" table
CREATE TABLE `event_streams` (`session_name` text NULL, `generation` text NOT NULL, PRIMARY KEY (`session_name`));
-- create "events" table
CREATE TABLE `events` (`event_id` text NULL, `session_name` text NOT NULL, `sequence` integer NOT NULL, `recorded_at` text NOT NULL, `type` text NOT NULL, `source` text NOT NULL, `direction` text NOT NULL, `summary` text NOT NULL, `body` text NOT NULL DEFAULT '', `metadata_json` text NOT NULL, `delivery_mode` text NOT NULL, PRIMARY KEY (`event_id`), CONSTRAINT `0` FOREIGN KEY (`session_name`) REFERENCES `event_streams` (`session_name`) ON UPDATE NO ACTION ON DELETE NO ACTION, CHECK (sequence > 0), UNIQUE (`session_name`, `sequence`));
-- create index "events_stream_sequence_idx" to table: "events"
CREATE INDEX `events_stream_sequence_idx` ON `events` (`session_name`, `sequence`);
-- create index "events_stream_id_idx" to table: "events"
CREATE INDEX `events_stream_id_idx` ON `events` (`session_name`, `event_id`);
-- create "event_consumer_positions" table
CREATE TABLE `event_consumer_positions` (`session_name` text NOT NULL, `consumer_name` text NOT NULL, `next_sequence` integer NOT NULL, PRIMARY KEY (`session_name`, `consumer_name`), CONSTRAINT `0` FOREIGN KEY (`session_name`) REFERENCES `event_streams` (`session_name`) ON UPDATE NO ACTION ON DELETE NO ACTION, CHECK (next_sequence >= 0));
-- create "event_watermarks" table
CREATE TABLE `event_watermarks` (`session_name` text NOT NULL, `watermark_name` text NOT NULL, `next_sequence` integer NOT NULL, PRIMARY KEY (`session_name`, `watermark_name`), CONSTRAINT `0` FOREIGN KEY (`session_name`) REFERENCES `event_streams` (`session_name`) ON UPDATE NO ACTION ON DELETE NO ACTION, CHECK (next_sequence >= 0));

-- +goose Down
-- reverse: create "event_watermarks" table
DROP TABLE `event_watermarks`;
-- reverse: create "event_consumer_positions" table
DROP TABLE `event_consumer_positions`;
-- reverse: create index "events_stream_id_idx" to table: "events"
DROP INDEX `events_stream_id_idx`;
-- reverse: create index "events_stream_sequence_idx" to table: "events"
DROP INDEX `events_stream_sequence_idx`;
-- reverse: create "events" table
DROP TABLE `events`;
-- reverse: create "event_streams" table
DROP TABLE `event_streams`;
