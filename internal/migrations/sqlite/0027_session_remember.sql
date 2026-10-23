-- "Remember me": a session records whether it was asked to last longer, when
-- the user last proved who they are (password and second factor) and a short
-- hint of where from (a /24 or /64 prefix, not the address). Sessions that
-- exist keep working: not remembered, authenticated when they began (0 means so).
ALTER TABLE sessions ADD COLUMN remember INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN authenticated_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN ip_hint TEXT NOT NULL DEFAULT '';
