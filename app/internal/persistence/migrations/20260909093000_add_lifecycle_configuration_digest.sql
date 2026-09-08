-- +goose Up
ALTER TABLE `sessions` ADD COLUMN `lifecycle_configuration_digest` text NULL;

-- +goose Down
ALTER TABLE `sessions` DROP COLUMN `lifecycle_configuration_digest`;
