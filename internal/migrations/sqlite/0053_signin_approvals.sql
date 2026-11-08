-- The user's own Telegram account, which approves their sign-ins in its private
-- chat with the bot. 0 means none is linked.
ALTER TABLE users ADD COLUMN telegram_user_id BIGINT NOT NULL DEFAULT 0;
-- How sign-ins are approved: '' (not needed), 'tap' or 'pin'.
ALTER TABLE users ADD COLUMN signin_method TEXT NOT NULL DEFAULT '';
-- The sign-in approval PIN, encrypted; set only while signin_method is 'pin'.
ALTER TABLE users ADD COLUMN signin_pin_enc BLOB;

-- A one-time link that connects a user's Telegram account. Only its hash is kept.
CREATE TABLE telegram_links (
    code_hash TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at BIGINT NOT NULL
);
CREATE INDEX telegram_links_user ON telegram_links (user_id);

-- One request per sign-in waiting for approval. The reply text is never stored.
CREATE TABLE signin_approvals (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    browser_hash TEXT NOT NULL UNIQUE,
    ip TEXT NOT NULL,
    status TEXT NOT NULL,
    method TEXT NOT NULL,
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

-- Messages that could hold a PIN and are still to be deleted, after a failed try.
CREATE TABLE signin_deletions (
    org_id TEXT NOT NULL,
    chat_id BIGINT NOT NULL,
    message_id BIGINT NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (chat_id, message_id)
);
