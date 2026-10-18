-- Error and warning logs, kept for debugging: application records at warn and
-- error level, failed provider calls (with redacted request and response
-- bodies), 5xx and rejected API/webhook requests, and failed jobs. Info and
-- debug are never stored. Nothing here is a credential or a customer's
-- personal data: every field is redacted before it is written.
--
-- On PostgreSQL this table lives in the main database. On SQLite the same
-- statements run in a separate file, <data>/logs.db, so log writes never take
-- the main database's write lock; the (empty) copy in paymux.db only keeps the
-- two dialects' schemas identical. The file can be deleted at any time.
CREATE TABLE logs (
    id                  TEXT PRIMARY KEY,
    at                  BIGINT NOT NULL,
    level               TEXT NOT NULL,
    kind                TEXT NOT NULL,
    component           TEXT NOT NULL,
    msg                 TEXT NOT NULL,
    request_id          TEXT,
    job_id              TEXT,
    org_id              TEXT,
    app_id              TEXT,
    payment_id          TEXT,
    attempt_id          TEXT,
    refund_id           TEXT,
    subscription_id     TEXT,
    provider_account_id TEXT,
    connector           TEXT,
    ids                 TEXT,
    attrs               TEXT,
    request             TEXT,
    response            TEXT,
    status              BIGINT,
    duration_ms         BIGINT
);
CREATE INDEX logs_at ON logs (at);
CREATE INDEX logs_level_at ON logs (level, at);
CREATE INDEX logs_kind_at ON logs (kind, at);
CREATE INDEX logs_request ON logs (request_id);
CREATE INDEX logs_payment ON logs (payment_id);
CREATE INDEX logs_attempt ON logs (attempt_id);
CREATE INDEX logs_refund ON logs (refund_id);
CREATE INDEX logs_subscription ON logs (subscription_id);
CREATE INDEX logs_job ON logs (job_id);
CREATE INDEX logs_account_at ON logs (provider_account_id, at);
