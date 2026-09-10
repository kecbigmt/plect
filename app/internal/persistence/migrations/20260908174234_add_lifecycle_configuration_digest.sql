-- +goose Up
-- add column "lifecycle_configuration_digest" to table: "sessions"
ALTER TABLE `sessions` ADD COLUMN `lifecycle_configuration_digest` text NULL;

-- +goose Down
-- reverse: add column "lifecycle_configuration_digest" to table: "sessions"
ALTER TABLE `sessions` DROP COLUMN `lifecycle_configuration_digest`;
