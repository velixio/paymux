CREATE TABLE notification_channels (
    org_id             TEXT NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('test','live')),
    email               TEXT,
    email_enabled       INTEGER NOT NULL DEFAULT 0,
    telegram_chat_id    TEXT,
    telegram_token_enc  BYTEA,
    telegram_enabled    INTEGER NOT NULL DEFAULT 0,
    event_types         TEXT NOT NULL DEFAULT '[]',
    updated_at          BIGINT NOT NULL,
    PRIMARY KEY (org_id, mode)
);
