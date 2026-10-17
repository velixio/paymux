-- Pay-page attempt reuse: the checkout's next action is kept on the attempt so
-- a reload re-renders it instead of creating another provider order.
ALTER TABLE payment_attempts ADD COLUMN next_action TEXT;
ALTER TABLE payment_attempts ADD COLUMN checkout_expires_at BIGINT;
