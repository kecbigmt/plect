-- +goose Up
-- create "persistence_smoke" table
CREATE TABLE `persistence_smoke` (`id` integer NULL, `note` text NOT NULL, `created_at` text NOT NULL, PRIMARY KEY (`id`));

-- +goose Down
-- reverse: create "persistence_smoke" table
DROP TABLE `persistence_smoke`;
