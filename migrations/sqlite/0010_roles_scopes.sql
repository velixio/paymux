-- Custom roles (the authz role -> actions map moves into the database), API
-- key scopes and key expiry (rotation).

CREATE TABLE roles (
    name       TEXT PRIMARY KEY,
    builtin    INTEGER NOT NULL DEFAULT 0,
    actions    TEXT NOT NULL, -- JSON array of authz actions
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);

-- The built-in roles (admin, operator, viewer) are defined in code and listed
-- here by the first start (core.Bootstrap), so this table copies between
-- databases like any other.

-- users.role was limited to the three built-in roles by a CHECK; SQLite cannot
-- drop a constraint, so the table is rebuilt.
CREATE TABLE users_new (
    id              TEXT PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    totp_secret_enc BLOB,
    totp_last_step  BIGINT NOT NULL,
    role            TEXT NOT NULL,
    disabled        INTEGER NOT NULL,
    created_at      BIGINT NOT NULL
);
INSERT INTO users_new (id, email, password_hash, totp_secret_enc, totp_last_step, role, disabled, created_at)
    SELECT id, email, password_hash, totp_secret_enc, totp_last_step, role, disabled, created_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

-- NULL scopes means every scope (all existing keys); expires_at is set while a
-- rotated-out key still works.
ALTER TABLE api_keys ADD COLUMN scopes TEXT;
ALTER TABLE api_keys ADD COLUMN expires_at BIGINT;
