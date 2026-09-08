-- +goose Up
CREATE TABLE chain_attempts (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    instance TEXT NOT NULL,
    chain_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    PRIMARY KEY (session_id, instance, chain_id)
);

CREATE TABLE subscription_retries (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    action TEXT NOT NULL CHECK (action IN ('subscribe', 'unsubscribe')),
    resource_id TEXT NOT NULL,
    PRIMARY KEY (session_id, action, resource_id)
);

-- +goose Down
DROP TABLE subscription_retries;
DROP TABLE chain_attempts;
