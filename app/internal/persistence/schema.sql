-- schema.sql is the hand-edited declarative authority for the database
-- structure described in docs/design/sqlite-persistence.md. Generate
-- migrations from it with Atlas Community Edition; do not hand-write
-- migration SQL against a structural change already captured here.
--
-- This bootstrap slice declares only the smoke table needed to prove the
-- schema.sql -> Atlas -> goose -> sqlite pipeline and the sqlc -> Go
-- pipeline end to end. Domain tables (sessions, task_instances, events,
-- and the rest of the inventory in the design doc) belong to later slices.
CREATE TABLE persistence_smoke (
    id INTEGER PRIMARY KEY,
    note TEXT NOT NULL,
    created_at TEXT NOT NULL
);
