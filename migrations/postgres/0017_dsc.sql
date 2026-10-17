-- A digital signature certificate (DSC) for signing invoice PDFs (PAdES). The
-- PKCS#12 file and its password are kept sealed in dsc_enc; the subject, the
-- expiry and the fingerprint are readable, for the dashboard.
ALTER TABLE tax_entities ADD COLUMN dsc_enc BYTEA;
ALTER TABLE tax_entities ADD COLUMN dsc_subject TEXT NOT NULL DEFAULT '';
ALTER TABLE tax_entities ADD COLUMN dsc_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE tax_entities ADD COLUMN dsc_not_after BIGINT;
ALTER TABLE tax_entities ADD COLUMN dsc_uploaded_at BIGINT;
