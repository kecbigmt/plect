-- +goose Up
-- Edited in place, a one-time exception to the append-only rule: unreleased,
-- so correcting this migration's cursor-kind value costs no second migration.
-- disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- create "new_event_cursors" table
CREATE TABLE `new_event_cursors` (`session_id` text NOT NULL, `kind` text NOT NULL, `next_sequence` integer NOT NULL, PRIMARY KEY (`session_id`, `kind`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (kind IN ('delivery', 'tick', 'heartbeat', 'forward')), CHECK (next_sequence >= 0));
-- copy rows from old table "event_cursors" to new temporary table "new_event_cursors"
INSERT INTO `new_event_cursors` (`session_id`, `kind`, `next_sequence`) SELECT `session_id`, `kind`, `next_sequence` FROM `event_cursors`;
-- drop "event_cursors" table after copying rows
DROP TABLE `event_cursors`;
-- rename temporary table "new_event_cursors" to "event_cursors"
ALTER TABLE `new_event_cursors` RENAME TO `event_cursors`;
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- +goose Down
-- Atlas's own generated Down drops the post-rename table under its
-- pre-rename name, which no longer exists once Up finishes -- hand-written
-- here as the same rebuild in reverse instead, mirroring
-- 20260907085032_add_node_execution_identity.sql's own `old_`-named rebuild.
PRAGMA foreign_keys = off;
CREATE TABLE `old_event_cursors` (`session_id` text NOT NULL, `kind` text NOT NULL, `next_sequence` integer NOT NULL, PRIMARY KEY (`session_id`, `kind`), CONSTRAINT `0` FOREIGN KEY (`session_id`) REFERENCES `sessions` (`id`) ON UPDATE NO ACTION ON DELETE CASCADE, CHECK (kind IN ('delivery', 'tick', 'heartbeat')), CHECK (next_sequence >= 0));
-- A row this Up ever created with kind='forward' would violate the
-- narrower CHECK above; Down is not lossless (see 20260906231449's own
-- precedent), so it drops that consumer's cursor rather than erroring.
INSERT INTO `old_event_cursors` (`session_id`, `kind`, `next_sequence`) SELECT `session_id`, `kind`, `next_sequence` FROM `event_cursors` WHERE `kind` != 'forward';
DROP TABLE `event_cursors`;
ALTER TABLE `old_event_cursors` RENAME TO `event_cursors`;
PRAGMA foreign_keys = on;
