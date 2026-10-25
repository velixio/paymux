-- H-13: the ledger's invariants are enforced by the database.
--
-- This migration refuses to run, and changes nothing, on a database where an
-- existing row already breaks one of them: adding a constraint checks the rows
-- that exist, and fails with the name of the rule.

-- Amounts. A refund the provider made on its own can exceed what was received
-- (the payment is flagged refund_exceeds_balance), so amount_refunded has no
-- upper bound.
ALTER TABLE payments ADD CONSTRAINT payments_amount_positive CHECK (amount > 0);
ALTER TABLE payments ADD CONSTRAINT payments_amount_refunded_not_negative CHECK (amount_refunded >= 0);
ALTER TABLE refunds ADD CONSTRAINT refunds_amount_positive CHECK (amount > 0);
ALTER TABLE payouts ADD CONSTRAINT payouts_amount_positive CHECK (amount > 0);

-- The attempt a ledger entry belongs to, so the extra charge of a double
-- payment (and its refund) can be told apart from the sale's. Entries from
-- before are matched where that is unambiguous and stay NULL otherwise.
ALTER TABLE ledger_entries ADD COLUMN attempt_id TEXT;
CREATE INDEX ledger_entries_attempt ON ledger_entries (attempt_id);

UPDATE ledger_entries SET attempt_id = (SELECT r.attempt_id FROM refunds r WHERE r.id = ledger_entries.refund_id)
WHERE refund_id IS NOT NULL;
UPDATE ledger_entries SET attempt_id = (SELECT d.attempt_id FROM disputes d WHERE d.id = ledger_entries.dispute_id)
WHERE dispute_id IS NOT NULL;
UPDATE ledger_entries SET attempt_id = (SELECT MIN(a.id) FROM payment_attempts a
    WHERE a.payment_id = ledger_entries.payment_id AND a.settlement_id = ledger_entries.settlement_id)
WHERE kind IN ('fee','fee_tax') AND payment_id IS NOT NULL AND settlement_id IS NOT NULL;
-- A charge is written in the transaction that captures its attempt, at the
-- same instant; failing that, the payment's only captured attempt.
UPDATE ledger_entries SET attempt_id = (SELECT MIN(a.id) FROM payment_attempts a
    WHERE a.payment_id = ledger_entries.payment_id AND a.amount_received > 0 AND a.updated_at = ledger_entries.occurred_at)
WHERE kind = 'charge' AND attempt_id IS NULL AND payment_id IS NOT NULL AND (SELECT COUNT(*) FROM payment_attempts a
    WHERE a.payment_id = ledger_entries.payment_id AND a.amount_received > 0 AND a.updated_at = ledger_entries.occurred_at) = 1;
UPDATE ledger_entries SET attempt_id = (SELECT MIN(a.id) FROM payment_attempts a
    WHERE a.payment_id = ledger_entries.payment_id AND a.amount_received > 0)
WHERE kind = 'charge' AND attempt_id IS NULL AND payment_id IS NOT NULL AND (SELECT COUNT(*) FROM payment_attempts a
    WHERE a.payment_id = ledger_entries.payment_id AND a.amount_received > 0) = 1;

-- A refund is booked once, and a dispute releases and loses once. A hold can be
-- booked twice: a lost dispute tops up a hold that was capped (A-13).
-- These fail, listing nothing but the name, on existing duplicates:
--   SELECT refund_id, kind, COUNT(*) FROM ledger_entries WHERE refund_id IS NOT NULL GROUP BY 1, 2 HAVING COUNT(*) > 1;
--   SELECT dispute_id, kind, COUNT(*) FROM ledger_entries WHERE dispute_id IS NOT NULL AND kind <> 'dispute_hold' GROUP BY 1, 2 HAVING COUNT(*) > 1;
CREATE UNIQUE INDEX ledger_entries_refund_once ON ledger_entries (refund_id, kind) WHERE refund_id IS NOT NULL;
CREATE UNIQUE INDEX ledger_entries_dispute_once ON ledger_entries (dispute_id, kind) WHERE dispute_id IS NOT NULL AND kind <> 'dispute_hold';

-- The ledger is append-only.
CREATE FUNCTION ledger_entries_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only';
END;
$$;
CREATE TRIGGER ledger_entries_no_change BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();
