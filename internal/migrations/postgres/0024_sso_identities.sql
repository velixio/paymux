-- Single sign-on is tied to the identity provider's (issuer, subject), not to
-- an e-mail address, and an account that has a password is only reachable by
-- single sign-on after an admin has said so.
CREATE TABLE user_identities (
    issuer     TEXT   NOT NULL,
    subject    TEXT   NOT NULL,
    user_id    TEXT   NOT NULL REFERENCES users(id),
    created_at BIGINT NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX user_identities_user ON user_identities (user_id);

CREATE TABLE user_sso_links (
    user_id    TEXT   PRIMARY KEY REFERENCES users(id),
    allowed_by TEXT   NOT NULL,
    created_at BIGINT NOT NULL
);
