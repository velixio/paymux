-- A single-use payment link remembers which payment its holder is paying, so
-- the same visitor submitting the page again gets that payment back instead of
-- a second one, and which payment spent it, so a later success on the same
-- link is recognised as a second sale.
ALTER TABLE payment_links ADD COLUMN claim_payment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE payment_links ADD COLUMN used_by_payment_id TEXT NOT NULL DEFAULT '';
