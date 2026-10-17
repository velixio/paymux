-- Disputes (spec 4, 6.7) and reconciliation runs and items (spec 11).

CREATE TABLE disputes (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL,
    app_id              TEXT NOT NULL,
    mode                TEXT NOT NULL CHECK (mode IN ('test','live')),
    payment_id          TEXT NOT NULL,
    attempt_id          TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    provider_dispute_id TEXT NOT NULL,
    amount              BIGINT NOT NULL,
    currency            TEXT NOT NULL,
    reason              TEXT,
    status              TEXT NOT NULL CHECK (status IN ('needs_response','under_review','won','lost','accepted','closed')),
    stage               TEXT,
    due_by              BIGINT,
    source              TEXT NOT NULL CHECK (source IN ('webhook','poll','manual')),
    evidence            TEXT NOT NULL DEFAULT '{}',
    version             BIGINT NOT NULL DEFAULT 1,
    alert_open_at       BIGINT,
    alert_72h_at        BIGINT,
    alert_24h_at        BIGINT,
    alert_lost_at       BIGINT,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL,
    UNIQUE (provider_account_id, provider_dispute_id)
);
CREATE INDEX disputes_payment ON disputes (payment_id);
CREATE INDEX disputes_open ON disputes (status, due_by);

CREATE TABLE recon_runs (
    id                  TEXT PRIMARY KEY,
    org_id              TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    window_from         BIGINT NOT NULL,
    window_to           BIGINT NOT NULL,
    status              TEXT NOT NULL CHECK (status IN ('running','ok','failed')),
    checked             INTEGER NOT NULL DEFAULT 0,
    items               INTEGER NOT NULL DEFAULT 0,
    fixed               INTEGER NOT NULL DEFAULT 0,
    error               TEXT,
    started_at          BIGINT NOT NULL,
    finished_at         BIGINT
);
CREATE INDEX recon_runs_account ON recon_runs (provider_account_id, started_at);

CREATE TABLE recon_items (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL,
    org_id              TEXT NOT NULL,
    provider_account_id TEXT NOT NULL,
    kind                TEXT NOT NULL,
    provider_id         TEXT NOT NULL,
    ours                TEXT,
    theirs              TEXT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN ('open','fixed','to_link')),
    created_at          BIGINT NOT NULL,
    UNIQUE (provider_account_id, kind, provider_id)
);
CREATE INDEX recon_items_run ON recon_items (run_id);
