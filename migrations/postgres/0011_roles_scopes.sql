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

ALTER TABLE users DROP CONSTRAINT users_role_check;

-- NULL scopes means every scope (all existing keys); expires_at is set while a
-- rotated-out key still works.
ALTER TABLE api_keys ADD COLUMN scopes TEXT;
ALTER TABLE api_keys ADD COLUMN expires_at BIGINT;
