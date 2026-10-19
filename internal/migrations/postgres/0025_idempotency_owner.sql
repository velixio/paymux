-- Each reservation of an idempotency key carries a token of the request that
-- made it, so a request taken over as stale cannot later complete or release
-- the reservation of the request that took over.
ALTER TABLE idempotency_keys ADD COLUMN owner TEXT NOT NULL DEFAULT '';
