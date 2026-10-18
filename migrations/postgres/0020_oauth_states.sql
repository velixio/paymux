-- OAuth authorizations in flight: the state sent to the provider and back is
-- single use, ten minutes, and bound to the user who began it and the account.
-- Only its hash is kept.
CREATE TABLE oauth_states (
    state_hash TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    account_id TEXT NOT NULL,
    expires_at BIGINT NOT NULL
);
CREATE INDEX oauth_states_expires ON oauth_states (expires_at);
