-- A user who has not yet set up two-factor sign-in can do so only with a
-- one-time code an admin issued; whoever merely knows the password is not
-- shown the secret. Only the hash of the code is stored.
CREATE TABLE user_enrolment_tokens (
    user_id        TEXT   PRIMARY KEY REFERENCES users(id),
    token_hash     TEXT   NOT NULL,
    expires_at     BIGINT NOT NULL,
    unlocked_until BIGINT NOT NULL DEFAULT 0
);
