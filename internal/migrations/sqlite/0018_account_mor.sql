-- Whether the provider sells as merchant of record is a fact of the account: a
-- remote connector learns it by discovery, which its registry entry cannot
-- say. It is recorded when the account is created; invoicing reads it.
ALTER TABLE provider_accounts ADD COLUMN merchant_of_record INTEGER NOT NULL DEFAULT 0;
