-- +goose Up
-- disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- drop "persistence_smoke" table
DROP TABLE `persistence_smoke`;
-- enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;

-- +goose Down
-- reverse: drop "persistence_smoke" table
CREATE TABLE `persistence_smoke` (`id` integer NULL, `note` text NOT NULL, `created_at` text NOT NULL, PRIMARY KEY (`id`));
