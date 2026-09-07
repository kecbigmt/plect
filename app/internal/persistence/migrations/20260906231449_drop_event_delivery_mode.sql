-- +goose Up
-- Atlas's own generated diff for this drop rebuilds the whole table (SQLite's
-- 12-step ALTER TABLE pattern) and computes a Down that drops the post-rename
-- `events` table under its pre-rename name `new_events`, which no longer
-- exists once Up finishes -- a no-op Down is preferable to a Down that errors
-- outright, so this migration is hand-written as a plain column drop/add
-- instead: it reaches the identical structural end state schema.sql declares
-- (verified by TestSchemaSQL_MatchesMigrationHistory), so the regen_check CI
-- step's `atlas migrate diff` finds no further diff against it.
ALTER TABLE `events` DROP COLUMN `delivery_mode`;

-- +goose Down
ALTER TABLE `events` ADD COLUMN `delivery_mode` text NOT NULL DEFAULT '';
