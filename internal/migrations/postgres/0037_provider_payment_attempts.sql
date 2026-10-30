CREATE TABLE provider_payment_attempts (
    provider_account_id TEXT NOT NULL,
    provider_payment_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    provider_order_id TEXT,
    status TEXT NOT NULL,
    method TEXT,
    failure_code TEXT,
    failure_message TEXT,
    amount BIGINT NOT NULL DEFAULT 0,
    currency TEXT,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (provider_account_id, provider_payment_id),
    FOREIGN KEY (attempt_id) REFERENCES payment_attempts(id)
);

CREATE INDEX provider_payment_attempts_attempt ON provider_payment_attempts (attempt_id, created_at);
