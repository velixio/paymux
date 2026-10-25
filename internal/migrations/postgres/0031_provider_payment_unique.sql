-- One provider payment settles one attempt. Two concurrent returns can both pass
-- the read check that a provider payment is not another attempt's; this index
-- closes that race. Duplicate successes (a second captured attempt of a payment
-- that was already paid) are recorded with duplicate = 1 and stay allowed.
--
-- The migration fails, and changes nothing, on a database where two such
-- attempts of one account already share a provider payment id. List them with:
--   SELECT provider_account_id, provider_payment_id, COUNT(*) FROM payment_attempts
--   WHERE status = 'captured' AND duplicate = 0 AND provider_payment_id IS NOT NULL
--   GROUP BY 1, 2 HAVING COUNT(*) > 1;
CREATE UNIQUE INDEX payment_attempts_provider_payment_captured ON payment_attempts (provider_account_id, provider_payment_id)
    WHERE status = 'captured' AND duplicate = 0 AND provider_payment_id IS NOT NULL;
