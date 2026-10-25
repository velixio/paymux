-- What a dispute holds back from its payment, which is the provider's amount
-- unless other open disputes on the same attempt already hold part of what was
-- received (A-13). Release and loss move exactly this amount, so the ledger of
-- a dispute always nets to zero. Existing disputes booked their full amount.
ALTER TABLE disputes ADD COLUMN amount_held BIGINT NOT NULL DEFAULT 0;
UPDATE disputes SET amount_held = amount;
