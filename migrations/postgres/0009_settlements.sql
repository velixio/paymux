-- Document sequence numbers (a stable listing order for documents issued in
-- the same millisecond) and settlement reconciliation (spec 11).

ALTER TABLE invoices ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
ALTER TABLE credit_notes ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
UPDATE invoices SET seq = CAST(regexp_replace(number, '^.*/', '') AS BIGINT);
UPDATE credit_notes SET seq = CAST(regexp_replace(number, '^.*/', '') AS BIGINT);
CREATE INDEX invoices_payment_seq ON invoices (payment_id, issued_at, seq);
CREATE INDEX credit_notes_invoice_seq ON credit_notes (invoice_id, issued_at, seq);

CREATE TABLE settlements (
    id                     TEXT PRIMARY KEY,
    org_id                 TEXT NOT NULL,
    mode                   TEXT NOT NULL CHECK (mode IN ('test','live')),
    provider_account_id    TEXT NOT NULL,
    provider_settlement_id TEXT NOT NULL,
    amount                 BIGINT NOT NULL,
    fees                   BIGINT NOT NULL DEFAULT 0,
    fee_tax                BIGINT NOT NULL DEFAULT 0,
    currency               TEXT NOT NULL,
    status                 TEXT NOT NULL,
    utr                    TEXT,
    settled_at             BIGINT,
    item_fees              BIGINT NOT NULL DEFAULT 0,
    item_fee_tax           BIGINT NOT NULL DEFAULT 0,
    fee_mismatch           INTEGER NOT NULL DEFAULT 0,
    payout_recorded        INTEGER NOT NULL DEFAULT 0,
    created_at             BIGINT NOT NULL,
    updated_at             BIGINT NOT NULL,
    UNIQUE (provider_account_id, provider_settlement_id)
);
CREATE INDEX settlements_settled ON settlements (org_id, settled_at);

ALTER TABLE payment_attempts ADD COLUMN settlement_id TEXT;
CREATE INDEX payment_attempts_settlement ON payment_attempts (settlement_id);
