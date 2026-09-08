-- +goose Up
CREATE TABLE chain_attempts (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    instance TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    generation TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    PRIMARY KEY (session_id, instance, chain_id, generation)
);

CREATE TABLE subscription_retries (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('subscribe', 'unsubscribe')),
    resource TEXT NOT NULL,
    PRIMARY KEY (session_id, action, resource)
);

-- +goose Down
DROP TABLE subscription_retries;
DROP TABLE chain_attempts;
