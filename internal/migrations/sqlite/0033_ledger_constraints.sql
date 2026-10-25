-- H-13: the ledger's invariants are enforced by the database.
--
-- This migration refuses to run, and changes nothing, on a database where an
-- existing row already breaks one of them. Each check below is a query that
-- lists the rows; the scratch tables make a violation fail the migration with
-- the name of the rule. SQLite cannot add a CHECK to a table, so the rules are
-- triggers there.

CREATE TABLE h13_payments_amount_must_be_positive (x INTEGER CHECK (x = 0));
INSERT INTO h13_payments_amount_must_be_positive SELECT 1 FROM payments WHERE amount <= 0 LIMIT 1;
DROP TABLE h13_payments_amount_must_be_positive;

CREATE TABLE h13_payments_amount_refunded_must_not_be_negative (x INTEGER CHECK (x = 0));
INSERT INTO h13_payments_amount_refunded_must_not_be_negative SELECT 1 FROM payments WHERE amount_refunded < 0 LIMIT 1;
DROP TABLE h13_payments_amount_refunded_must_not_be_negative;

CREATE TABLE h13_refunds_amount_must_be_positive (x INTEGER CHECK (x = 0));
INSERT INTO h13_refunds_amount_must_be_positive SELECT 1 FROM refunds WHERE amount <= 0 LIMIT 1;
DROP TABLE h13_refunds_amount_must_be_positive;

CREATE TABLE h13_payouts_amount_must_be_positive (x INTEGER CHECK (x = 0));
INSERT INTO h13_payouts_amount_must_be_positive SELECT 1 FROM payouts WHERE amount <= 0 LIMIT 1;
DROP TABLE h13_payouts_amount_must_be_positive;

-- Amounts. A refund the provider made on its own can exceed what was received
-- (the payment is flagged refund_exceeds_balance), so amount_refunded has no
-- upper bound.
CREATE TRIGGER payments_amount_insert BEFORE INSERT ON payments
WHEN NEW.amount <= 0 OR NEW.amount_refunded < 0
BEGIN SELECT RAISE(ABORT, 'payments: amount must be positive and amount_refunded not negative'); END;
CREATE TRIGGER payments_amount_update BEFORE UPDATE ON payments
WHEN NEW.amount <= 0 OR NEW.amount_refunded < 0
BEGIN SELECT RAISE(ABORT, 'payments: amount must be positive and amount_refunded not negative'); END;

CREATE TRIGGER refunds_amount_insert BEFORE INSERT ON refunds
WHEN NEW.amount <= 0
BEGIN SELECT RAISE(ABORT, 'refunds: amount must be positive'); END;
CREATE TRIGGER refunds_amount_update BEFORE UPDATE ON refunds
WHEN NEW.amount <= 0
BEGIN SELECT RAISE(ABORT, 'refunds: amount must be positive'); END;

CREATE TRIGGER payouts_amount_insert BEFORE INSERT ON payouts
WHEN NEW.amount <= 0
BEGIN SELECT RAISE(ABORT, 'payouts: amount must be positive'); END;
CREATE TRIGGER payouts_amount_update BEFORE UPDATE ON payouts
WHEN NEW.amount <= 0
BEGIN SELECT RAISE(ABORT, 'payouts: amount must be positive'); END;

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
CREATE TRIGGER ledger_entries_no_update BEFORE UPDATE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger_entries is append-only'); END;
CREATE TRIGGER ledger_entries_no_delete BEFORE DELETE ON ledger_entries
BEGIN SELECT RAISE(ABORT, 'ledger_entries is append-only'); END;
