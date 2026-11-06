-- A user's own sign-in approval PIN, encrypted. NULL means sign-in needs no approval.
ALTER TABLE users ADD COLUMN signin_pin_enc BYTEA;

-- One request per sign-in waiting for approval. The reply text is never stored.
CREATE TABLE signin_approvals (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    browser_hash TEXT NOT NULL UNIQUE,
    ip TEXT NOT NULL,
    status TEXT NOT NULL,
    remember BIGINT NOT NULL DEFAULT 0,
    chat_id BIGINT NOT NULL DEFAULT 0,
    message_id BIGINT NOT NULL DEFAULT 0,
    wrong_attempts BIGINT NOT NULL DEFAULT 0,
    cleared BIGINT NOT NULL DEFAULT 0,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    decided_at BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX signin_approvals_status ON signin_approvals (status, expires_at);
CREATE INDEX signin_approvals_notification ON signin_approvals (chat_id, message_id);
CREATE INDEX signin_approvals_user ON signin_approvals (user_id, created_at);

-- How far replies have been read, per bot, and who is reading them now.
CREATE TABLE signin_polls (
    bot_id TEXT PRIMARY KEY,
    update_offset BIGINT NOT NULL DEFAULT 0,
    lease_until BIGINT NOT NULL DEFAULT 0
);
