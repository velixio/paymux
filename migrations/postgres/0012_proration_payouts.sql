-- Subscription proration (adjustments applied to renewals) and payouts.

-- proration_adjust is what the next renewal changes by (a charge when positive,
-- a credit when negative); proration_lines explain it for the invoice;
-- billed_amount is the rate the current period is paid at after a prorated
-- change (0: the subscription's amount).
ALTER TABLE subscriptions ADD COLUMN proration_adjust BIGINT NOT NULL DEFAULT 0;
ALTER TABLE subscriptions ADD COLUMN proration_lines TEXT NOT NULL DEFAULT '[]';
ALTER TABLE subscriptions ADD COLUMN billed_amount BIGINT NOT NULL DEFAULT 0;

-- The adjustment lines included in a cycle's amount (JSON), for its invoice.
ALTER TABLE subscription_cycles ADD COLUMN adjustments TEXT NOT NULL DEFAULT '[]';

CREATE TABLE payouts (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('test','live')),
    app_id             TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    provider_ref       TEXT NOT NULL UNIQUE,
    provider_payout_id TEXT,
    amount             BIGINT NOT NULL,
    currency           TEXT NOT NULL,
    status             TEXT NOT NULL,
    raw_status         TEXT NOT NULL DEFAULT '',
    beneficiary_enc    BYTEA NOT NULL,
    beneficiary_label  TEXT NOT NULL,
    narration          TEXT NOT NULL DEFAULT '',
    metadata           TEXT NOT NULL DEFAULT '{}',
    failure_code       TEXT NOT NULL DEFAULT '',
    failure_message    TEXT NOT NULL DEFAULT '',
    submit_attempts    INTEGER NOT NULL DEFAULT 0,
    version            BIGINT NOT NULL DEFAULT 1,
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL,
    completed_at       BIGINT
);
CREATE INDEX payouts_app_list ON payouts (app_id, mode, id);
