CREATE TABLE users (
    id              TEXT PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    totp_secret_enc BLOB,
    totp_last_step  BIGINT NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('admin','operator','viewer')),
    disabled        INTEGER NOT NULL,
    created_at      BIGINT NOT NULL
);

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    csrf_key     BLOB NOT NULL,
    created_at   BIGINT NOT NULL,
    last_seen_at BIGINT NOT NULL,
    expires_at   BIGINT NOT NULL,
    ip           TEXT NOT NULL,
    user_agent   TEXT NOT NULL
);

CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);
