-- One row per provider try. Some providers (Stripe) retry within one provider
-- payment id, so the id alone can no longer key this history.
CREATE TABLE provider_payment_attempts_new (
    provider_account_id TEXT NOT NULL,
    try_id TEXT NOT NULL,
    provider_payment_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    provider_order_id TEXT,
    status TEXT NOT NULL,
    method TEXT,
    failure_code TEXT,
    failure_message TEXT,
    amount BIGINT NOT NULL DEFAULT 0,
    currency TEXT,
    method_details TEXT,
    risk_details TEXT,
    three_d_secure TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (provider_account_id, try_id),
    FOREIGN KEY (attempt_id) REFERENCES payment_attempts(id)
);

INSERT INTO provider_payment_attempts_new
    (provider_account_id, try_id, provider_payment_id, attempt_id, provider_order_id, status, method,
     failure_code, failure_message, amount, currency, created_at, updated_at)
SELECT provider_account_id, provider_payment_id, provider_payment_id, attempt_id, provider_order_id, status, method,
       failure_code, failure_message, amount, currency, created_at, updated_at
FROM provider_payment_attempts;

DROP TABLE provider_payment_attempts;
ALTER TABLE provider_payment_attempts_new RENAME TO provider_payment_attempts;

CREATE INDEX provider_payment_attempts_attempt ON provider_payment_attempts (attempt_id, created_at);
CREATE INDEX provider_payment_attempts_payment ON provider_payment_attempts (provider_account_id, provider_payment_id);
