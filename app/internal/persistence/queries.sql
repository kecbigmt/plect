-- name: InsertPersistenceSmoke :one
INSERT INTO persistence_smoke (note, created_at)
VALUES (?, ?)
RETURNING id, note, created_at;
