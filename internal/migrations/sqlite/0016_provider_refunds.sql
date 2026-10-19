-- A provider refund id names one refund of one attempt. Refunds the provider
-- made on its own (initiated_by = provider) are imported by that id, so the
-- same refund arriving twice, by webhook and by reconciliation, is one row.
CREATE UNIQUE INDEX refunds_attempt_provider_refund_id ON refunds (attempt_id, provider_refund_id)
    WHERE provider_refund_id IS NOT NULL;
