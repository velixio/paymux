CREATE INDEX ledger_entries_org_mode_time ON ledger_entries (org_id, mode, occurred_at, kind, currency, amount);
