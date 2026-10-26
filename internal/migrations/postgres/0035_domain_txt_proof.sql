-- DNS proof of custom domains. txt_verified_at is when the _paymux-challenge
-- TXT record was last seen (a pending domain gets a certificate only after
-- that); grace_started_at is when an already active domain was first found
-- without the record (it goes back to pending 7 days later). Both are NULL for
-- every existing row.
ALTER TABLE app_domains ADD COLUMN txt_verified_at BIGINT;
ALTER TABLE app_domains ADD COLUMN grace_started_at BIGINT;
