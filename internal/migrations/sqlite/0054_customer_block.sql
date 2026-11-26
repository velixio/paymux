-- A blocked customer cannot start a new attempt, and a payment of theirs that
-- succeeds goes to review. Set when their declined cards reach the
-- customer_decline_limit setting, or by an operator; cleared by an operator.
ALTER TABLE customers ADD COLUMN blocked_at BIGINT;
ALTER TABLE customers ADD COLUMN blocked_reason TEXT;
