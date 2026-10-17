-- GST invoicing: tax entities, per-app tax profiles, gapless document series,
-- invoices, credit notes, FX rates, send records and refund tax flags.
-- Money is int64 minor units of the document currency; times are unix ms UTC;
-- JSON is TEXT.

CREATE TABLE tax_entities (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    legal_name         TEXT NOT NULL,
    trade_name         TEXT NOT NULL DEFAULT '',
    gstin              TEXT NOT NULL,
    pan                TEXT NOT NULL DEFAULT '',
    address            TEXT NOT NULL DEFAULT '',
    state_code         TEXT NOT NULL,
    lut_number         TEXT,
    lut_valid_from     BIGINT,
    lut_valid_to       BIGINT,
    invoice_prefix     TEXT NOT NULL DEFAULT 'INV',
    credit_note_prefix TEXT NOT NULL DEFAULT 'CN',
    logo_blob_id       TEXT,
    signatory_name     TEXT NOT NULL DEFAULT '',
    footer_note        TEXT NOT NULL DEFAULT '',
    support_email      TEXT NOT NULL DEFAULT '',
    turnover_band      TEXT NOT NULL DEFAULT '',
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL,
    UNIQUE (org_id, gstin)
);

CREATE TABLE app_tax_profiles (
    app_id             TEXT PRIMARY KEY,
    tax_entity_id      TEXT NOT NULL,
    issue_invoices     INTEGER NOT NULL DEFAULT 1,
    sac_code           TEXT NOT NULL DEFAULT '998315',
    gst_rate_bp        INTEGER NOT NULL DEFAULT 1800,
    prices_include_tax INTEGER NOT NULL DEFAULT 1,
    invoice_prefix     TEXT,
    credit_note_prefix TEXT,
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL
);

CREATE TABLE document_series (
    tax_entity_id TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('invoice','credit_note')),
    prefix        TEXT NOT NULL,
    fiscal_year   TEXT NOT NULL,
    next_number   BIGINT NOT NULL DEFAULT 1,
    UNIQUE (tax_entity_id, kind, prefix, fiscal_year)
);

CREATE TABLE invoices (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL,
    mode              TEXT NOT NULL CHECK (mode IN ('test','live')),
    app_id            TEXT NOT NULL,
    tax_entity_id     TEXT NOT NULL,
    payment_id        TEXT NOT NULL,
    attempt_id        TEXT NOT NULL,
    subscription_id   TEXT,
    number            TEXT NOT NULL,
    series_prefix     TEXT NOT NULL,
    fiscal_year       TEXT NOT NULL,
    issued_at         BIGINT NOT NULL,
    supply_type       TEXT NOT NULL CHECK (supply_type IN ('b2b','b2c','export_lut','export_igst')),
    place_of_supply   TEXT NOT NULL,
    inter_state       INTEGER NOT NULL DEFAULT 0,
    currency          TEXT NOT NULL,
    supplier          TEXT NOT NULL,
    recipient         TEXT NOT NULL,
    lines             TEXT NOT NULL,
    taxable           BIGINT NOT NULL,
    cgst              BIGINT NOT NULL DEFAULT 0,
    sgst              BIGINT NOT NULL DEFAULT 0,
    igst              BIGINT NOT NULL DEFAULT 0,
    total             BIGINT NOT NULL,
    gst_rate_bp       INTEGER NOT NULL DEFAULT 0,
    fx_rate_inr       TEXT,
    fx_source         TEXT,
    flags             TEXT NOT NULL DEFAULT '[]',
    payment_method    TEXT NOT NULL DEFAULT '',
    payment_ref       TEXT NOT NULL DEFAULT '',
    lut_number        TEXT,
    template_version  TEXT NOT NULL,
    pdf_blob_id       TEXT,
    access_token_hash TEXT NOT NULL,
    UNIQUE (tax_entity_id, series_prefix, fiscal_year, number)
);
CREATE INDEX invoices_payment ON invoices (payment_id);
CREATE INDEX invoices_entity_issued ON invoices (tax_entity_id, issued_at);

CREATE TABLE credit_notes (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL,
    mode              TEXT NOT NULL CHECK (mode IN ('test','live')),
    app_id            TEXT NOT NULL,
    tax_entity_id     TEXT NOT NULL,
    invoice_id        TEXT NOT NULL,
    payment_id        TEXT NOT NULL,
    refund_id         TEXT,
    dispute_id        TEXT,
    number            TEXT NOT NULL,
    series_prefix     TEXT NOT NULL,
    fiscal_year       TEXT NOT NULL,
    issued_at         BIGINT NOT NULL,
    reason            TEXT NOT NULL DEFAULT '',
    currency          TEXT NOT NULL,
    lines             TEXT NOT NULL,
    taxable           BIGINT NOT NULL,
    cgst              BIGINT NOT NULL DEFAULT 0,
    sgst              BIGINT NOT NULL DEFAULT 0,
    igst              BIGINT NOT NULL DEFAULT 0,
    total             BIGINT NOT NULL,
    flags             TEXT NOT NULL DEFAULT '[]',
    template_version  TEXT NOT NULL,
    pdf_blob_id       TEXT,
    access_token_hash TEXT NOT NULL,
    UNIQUE (tax_entity_id, series_prefix, fiscal_year, number),
    UNIQUE (refund_id)
);
CREATE INDEX credit_notes_invoice ON credit_notes (invoice_id);
CREATE INDEX credit_notes_payment ON credit_notes (payment_id);
CREATE INDEX credit_notes_entity_issued ON credit_notes (tax_entity_id, issued_at);

CREATE TABLE fx_rates (
    org_id      TEXT NOT NULL,
    currency    TEXT NOT NULL,
    rate_date   TEXT NOT NULL,
    rate_to_inr TEXT NOT NULL,
    source      TEXT NOT NULL CHECK (source IN ('manual','provider')),
    created_at  BIGINT NOT NULL,
    PRIMARY KEY (org_id, currency, rate_date)
);

CREATE TABLE document_sends (
    id        TEXT PRIMARY KEY,
    doc_kind  TEXT NOT NULL CHECK (doc_kind IN ('invoice','credit_note')),
    doc_id    TEXT NOT NULL,
    recipient TEXT NOT NULL,
    status    TEXT NOT NULL,
    error     TEXT,
    sent_at   BIGINT NOT NULL
);
CREATE INDEX document_sends_doc ON document_sends (doc_kind, doc_id);

-- Tax flags on refunds (credit_note_cutoff_passed), kept apart from the
-- refunds table so the core lane owns that schema.
CREATE TABLE refund_flags (
    refund_id  TEXT NOT NULL,
    flag       TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (refund_id, flag)
);
