-- The Idempotency-Key of the request that created a payout or a refund lives on
-- the object itself, so a retry can never create a second one, even when the
-- middleware's reservation was taken over after it went stale. Rows made before
-- this have no key, and NULLs do not collide.
ALTER TABLE payouts ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX payouts_idempotency ON payouts (app_id, mode, idempotency_key);

ALTER TABLE refunds ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX refunds_idempotency ON refunds (app_id, mode, idempotency_key);
