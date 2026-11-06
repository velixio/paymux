ALTER TABLE payment_attempts ADD COLUMN risk_details TEXT NOT NULL DEFAULT '{}';
-- 0 until the risk backfill has asked the provider about this attempt.
ALTER TABLE payment_attempts ADD COLUMN risk_checked_at BIGINT NOT NULL DEFAULT 0;
CREATE INDEX idx_payment_attempts_risk_unchecked ON payment_attempts (provider_account_id, updated_at) WHERE risk_checked_at = 0;
