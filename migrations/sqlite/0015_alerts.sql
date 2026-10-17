-- Ops alerts center: every alert Paymux raises (provider silent, reconciliation
-- failed, dispute deadlines, webhook endpoints failing, ...) is kept here, so
-- that it can be seen and acknowledged in the dashboard in addition to the
-- email and webhook it is delivered by. A repeat of a condition that is still
-- open (same kind and refs) bumps `occurrences` instead of adding a row.
CREATE TABLE alerts (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    severity        TEXT NOT NULL CHECK (severity IN ('info','warning','critical')),
    kind            TEXT NOT NULL,
    message         TEXT NOT NULL,
    refs            TEXT NOT NULL DEFAULT '{}',
    fingerprint     TEXT NOT NULL,
    occurrences     INTEGER NOT NULL DEFAULT 1,
    raised_at       BIGINT NOT NULL,
    last_raised_at  BIGINT NOT NULL,
    acknowledged_by TEXT,
    acknowledged_at BIGINT
);
-- At most one open alert per condition.
CREATE UNIQUE INDEX alerts_open_fingerprint ON alerts (org_id, fingerprint) WHERE acknowledged_at IS NULL;
CREATE INDEX alerts_org_recent ON alerts (org_id, last_raised_at);
