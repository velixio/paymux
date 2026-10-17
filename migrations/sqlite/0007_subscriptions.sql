-- Subscriptions, mandates and the renewal cycles that drive them (spec 6.8).
-- Money is int64 minor units; times are unix ms UTC; JSON is TEXT.

CREATE TABLE mandates (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL,
    mode                TEXT NOT NULL CHECK (mode IN ('test','live')),
    app_id              TEXT NOT NULL,
    customer_id         TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    provider_ref        TEXT NOT NULL,
    provider_mandate_id TEXT,
    method              TEXT NOT NULL CHECK (method IN ('upi','card','emandate')),
    status              TEXT NOT NULL CHECK (status IN ('pending','active','revoked','expired','failed')),
    max_amount          BIGINT NOT NULL,
    currency            TEXT NOT NULL,
    frequency           TEXT NOT NULL DEFAULT '',
    valid_until         BIGINT,
    expiry_notified_at  BIGINT,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL,
    UNIQUE (provider_ref)
);
CREATE UNIQUE INDEX mandates_provider_id ON mandates (provider_account_id, provider_mandate_id)
    WHERE provider_mandate_id IS NOT NULL;
CREATE INDEX mandates_customer ON mandates (customer_id);

CREATE TABLE subscriptions (
    id                       TEXT PRIMARY KEY,
    org_id                   TEXT NOT NULL,
    mode                     TEXT NOT NULL CHECK (mode IN ('test','live')),
    app_id                   TEXT NOT NULL,
    customer_id              TEXT NOT NULL,
    status                   TEXT NOT NULL,
    billing_mode             TEXT NOT NULL CHECK (billing_mode IN ('provider_managed','merchant_triggered')),
    provider_account_id      TEXT NOT NULL,
    provider_subscription_id TEXT,
    provider_ref             TEXT NOT NULL,
    mandate_id               TEXT,
    pending_mandate_id       TEXT,
    amount                   BIGINT NOT NULL,
    currency                 TEXT NOT NULL,
    interval                 TEXT NOT NULL CHECK (interval IN ('day','week','month','year')),
    interval_count           INTEGER NOT NULL,
    anchor_at                BIGINT NOT NULL,
    current_period_start     BIGINT NOT NULL,
    current_period_end       BIGINT NOT NULL,
    trial_end                BIGINT,
    total_count              INTEGER,
    renewals_done            INTEGER NOT NULL DEFAULT 0,
    cancel_at_period_end     INTEGER NOT NULL DEFAULT 0,
    canceled_at              BIGINT,
    ended_at                 BIGINT,
    pending_amount           BIGINT,
    requires_reauthorization INTEGER NOT NULL DEFAULT 0,
    dunning_attempt          INTEGER NOT NULL DEFAULT 0,
    next_retry_at            BIGINT,
    description              TEXT NOT NULL DEFAULT '',
    metadata                 TEXT NOT NULL DEFAULT '{}',
    return_url               TEXT NOT NULL DEFAULT '',
    checkout_token_hash      TEXT NOT NULL,
    expires_at               BIGINT NOT NULL,
    version                  BIGINT NOT NULL DEFAULT 1,
    created_at               BIGINT NOT NULL,
    updated_at               BIGINT NOT NULL,
    UNIQUE (provider_ref)
);
CREATE INDEX subscriptions_app_list ON subscriptions (app_id, mode, id);
CREATE INDEX subscriptions_customer ON subscriptions (customer_id);
CREATE UNIQUE INDEX subscriptions_provider_id ON subscriptions (provider_account_id, provider_subscription_id)
    WHERE provider_subscription_id IS NOT NULL;
CREATE INDEX subscriptions_mandate ON subscriptions (mandate_id);

-- One row per billed period (merchant_triggered: period_index) or per provider
-- charge (provider_managed: provider_payment_id). The unique keys make a
-- renewal impossible to create twice; state and lease make a charge
-- impossible to run twice at once.
CREATE TABLE subscription_cycles (
    id                  TEXT PRIMARY KEY,
    subscription_id     TEXT NOT NULL,
    period_index        INTEGER,
    provider_payment_id TEXT,
    payment_id          TEXT,
    attempt_id          TEXT,
    amount              BIGINT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN ('scheduled','charging','awaiting','succeeded','failed','exhausted')),
    retries             INTEGER NOT NULL DEFAULT 0,
    job_seq             INTEGER NOT NULL DEFAULT 0,
    notified_at         BIGINT,
    next_run_at         BIGINT,
    lease_until         BIGINT,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL,
    UNIQUE (subscription_id, period_index),
    UNIQUE (subscription_id, provider_payment_id)
);
CREATE INDEX subscription_cycles_payment ON subscription_cycles (payment_id);
