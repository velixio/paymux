-- Core domain: apps, keys, customers, payments, attempts, refunds, ledger,
-- idempotency, audit, settings, blobs. Money is int64 minor units; times are
-- unix ms UTC; JSON is TEXT.

CREATE TABLE apps (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    name               TEXT NOT NULL,
    slug               TEXT NOT NULL,
    default_return_url TEXT,
    branding           TEXT NOT NULL DEFAULT '{}',
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL,
    UNIQUE (org_id, slug)
);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL,
    app_id       TEXT NOT NULL,
    mode         TEXT NOT NULL CHECK (mode IN ('test','live')),
    prefix       TEXT NOT NULL,
    sha256_hash  TEXT NOT NULL,
    last_used_at BIGINT,
    revoked_at   BIGINT,
    created_at   BIGINT NOT NULL,
    UNIQUE (prefix)
);
CREATE INDEX api_keys_app ON api_keys (app_id);

CREATE TABLE customers (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    app_id             TEXT NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('test','live')),
    external_ref       TEXT,
    name               TEXT,
    email              TEXT,
    phone              TEXT,
    billing_legal_name TEXT,
    billing_gstin      TEXT,
    billing_line1      TEXT,
    billing_line2      TEXT,
    billing_city       TEXT,
    billing_state_code TEXT,
    billing_postal_code TEXT,
    billing_country    TEXT,
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL
);
CREATE UNIQUE INDEX customers_external_ref ON customers (app_id, mode, external_ref)
    WHERE external_ref IS NOT NULL;

CREATE TABLE provider_accounts (
    id                   TEXT PRIMARY KEY,
    org_id               TEXT NOT NULL,
    mode                 TEXT NOT NULL CHECK (mode IN ('test','live')),
    connector            TEXT NOT NULL,
    name                 TEXT NOT NULL,
    config_enc           BYTEA,
    status               TEXT NOT NULL DEFAULT 'active',
    statement_descriptor TEXT,
    provider_gstin       TEXT,
    accepted_currencies  TEXT NOT NULL DEFAULT '[]',
    enabled_purposes     TEXT NOT NULL DEFAULT '["one_time"]',
    created_at           BIGINT NOT NULL,
    updated_at           BIGINT NOT NULL
);

CREATE TABLE app_provider_accounts (
    app_id              TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    PRIMARY KEY (app_id, provider_account_id)
);

CREATE TABLE payments (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    app_id             TEXT NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('test','live')),
    kind               TEXT NOT NULL CHECK (kind IN ('one_time','renewal','mandate_setup')),
    subscription_id    TEXT,
    amount             BIGINT NOT NULL,
    currency           TEXT NOT NULL,
    amount_received    BIGINT NOT NULL DEFAULT 0,
    amount_refunded    BIGINT NOT NULL DEFAULT 0,
    status             TEXT NOT NULL,
    order_ref          TEXT,
    description        TEXT,
    customer_id        TEXT,
    metadata           TEXT NOT NULL DEFAULT '{}',
    return_url         TEXT,
    client_secret_hash TEXT NOT NULL,
    expires_at         BIGINT NOT NULL,
    version            BIGINT NOT NULL DEFAULT 1,
    flags              TEXT NOT NULL DEFAULT '[]',
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL
);
CREATE INDEX payments_app_list ON payments (app_id, mode, id);
CREATE INDEX payments_order_ref ON payments (app_id, mode, order_ref);

CREATE TABLE payment_attempts (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL,
    mode                TEXT NOT NULL CHECK (mode IN ('test','live')),
    payment_id          TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    provider_ref        TEXT NOT NULL,
    provider_order_id   TEXT,
    provider_payment_id TEXT,
    status              TEXT NOT NULL,
    method              TEXT,
    failure_code        TEXT,
    failure_message     TEXT,
    raw_last            TEXT,
    amount_received     BIGINT NOT NULL DEFAULT 0,
    currency_received   TEXT,
    duplicate           INTEGER NOT NULL DEFAULT 0,
    fee                 BIGINT,
    fee_tax             BIGINT,
    net                 BIGINT,
    mandate_id          TEXT,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL,
    UNIQUE (provider_ref)
);
CREATE INDEX payment_attempts_payment ON payment_attempts (payment_id);

CREATE TABLE refunds (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    app_id             TEXT NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('test','live')),
    payment_id         TEXT NOT NULL,
    attempt_id         TEXT NOT NULL,
    amount             BIGINT NOT NULL,
    currency           TEXT NOT NULL,
    status             TEXT NOT NULL,
    reason             TEXT,
    provider_ref       TEXT NOT NULL,
    provider_refund_id TEXT,
    initiated_by       TEXT NOT NULL CHECK (initiated_by IN ('api','dashboard','provider','system')),
    actor              TEXT,
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL,
    UNIQUE (provider_ref)
);
CREATE INDEX refunds_payment ON refunds (payment_id);
CREATE INDEX refunds_attempt ON refunds (attempt_id);

CREATE TABLE ledger_entries (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL,
    mode          TEXT NOT NULL CHECK (mode IN ('test','live')),
    kind          TEXT NOT NULL CHECK (kind IN ('charge','refund','dispute_hold','dispute_release','dispute_loss','fee','fee_tax','payout')),
    amount        BIGINT NOT NULL,
    currency      TEXT NOT NULL,
    payment_id    TEXT,
    refund_id     TEXT,
    dispute_id    TEXT,
    settlement_id TEXT,
    occurred_at   BIGINT NOT NULL
);
CREATE INDEX ledger_entries_payment ON ledger_entries (payment_id);

CREATE TABLE idempotency_keys (
    app_id          TEXT NOT NULL,
    mode            TEXT NOT NULL CHECK (mode IN ('test','live')),
    key             TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    response_status INTEGER NOT NULL DEFAULT 0,
    response_enc    BYTEA,
    created_at      BIGINT NOT NULL,
    PRIMARY KEY (app_id, mode, key)
);

CREATE TABLE audit_log (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('user','api_key','system')),
    actor_id   TEXT,
    action     TEXT NOT NULL,
    target     TEXT,
    before     TEXT,
    after      TEXT,
    ip         TEXT,
    at         BIGINT NOT NULL
);
CREATE INDEX audit_log_at ON audit_log (org_id, at);

CREATE TABLE org_settings (
    org_id     TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (org_id, key)
);

CREATE TABLE app_settings (
    app_id     TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (app_id, key)
);

CREATE TABLE blobs (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL,
    kind         TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size         BIGINT NOT NULL,
    sha256       TEXT NOT NULL,
    storage      TEXT NOT NULL DEFAULT 'db',
    data         BYTEA,
    created_at   BIGINT NOT NULL
);
