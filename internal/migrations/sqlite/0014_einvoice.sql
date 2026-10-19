-- GST e-invoice, offline mode: the operator uploads the invoice JSON to the
-- Invoice Registration Portal by hand and records what it returns. The IRN is
-- unique across invoices so that one pasted twice is caught.
ALTER TABLE invoices ADD COLUMN irn TEXT;
ALTER TABLE invoices ADD COLUMN ack_no TEXT;
ALTER TABLE invoices ADD COLUMN ack_date BIGINT;
ALTER TABLE invoices ADD COLUMN signed_qr TEXT;
ALTER TABLE invoices ADD COLUMN einvoice_at BIGINT;
CREATE UNIQUE INDEX invoices_irn ON invoices (irn) WHERE irn IS NOT NULL;
