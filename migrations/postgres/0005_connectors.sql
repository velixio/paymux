-- Connector runtime: per-account key/value state, app custom domains,
-- inbound objects Paymux could not match, and polling bookkeeping.

CREATE TABLE connector_kv (
    provider_account_id TEXT NOT NULL,
    key                 TEXT NOT NULL,
    value_enc           BYTEA NOT NULL,
    expires_at          BIGINT,
    PRIMARY KEY (provider_account_id, key)
);

CREATE TABLE app_domains (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL,
    app_id        TEXT NOT NULL,
    host          TEXT NOT NULL,
    is_primary    INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL CHECK (status IN ('pending','active','failed')),
    token         TEXT NOT NULL,
    last_check_at BIGINT,
    last_error    TEXT,
    created_at    BIGINT NOT NULL,
    UNIQUE (host)
);
CREATE INDEX app_domains_app ON app_domains (app_id);

CREATE TABLE unknown_provider_objects (
    id                  TEXT PRIMARY KEY,
    provider_account_id TEXT NOT NULL,
    provider_event_id   TEXT NOT NULL,
    kind                TEXT NOT NULL,
    provider_id         TEXT NOT NULL,
    detail              TEXT,
    created_at          BIGINT NOT NULL,
    UNIQUE (provider_account_id, provider_event_id, kind, provider_id)
);

ALTER TABLE payment_attempts ADD COLUMN poll_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE payment_attempts ADD COLUMN next_poll_at BIGINT;
ALTER TABLE refunds ADD COLUMN poll_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE refunds ADD COLUMN next_poll_at BIGINT;

CREATE INDEX payment_attempts_provider_order ON payment_attempts (provider_account_id, provider_order_id);
CREATE INDEX payment_attempts_provider_payment ON payment_attempts (provider_account_id, provider_payment_id);
CREATE INDEX refunds_provider_refund ON refunds (provider_refund_id);
