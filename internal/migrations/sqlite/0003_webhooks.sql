CREATE TABLE events (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    app_id         TEXT NOT NULL,
    mode           TEXT NOT NULL,
    type           TEXT NOT NULL,
    object_type    TEXT NOT NULL,
    object_id      TEXT NOT NULL,
    object_version BIGINT NOT NULL,
    data           TEXT NOT NULL,
    created_at     BIGINT NOT NULL
);

CREATE INDEX events_app_created ON events (app_id, created_at);

CREATE TABLE webhook_endpoints (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    app_id             TEXT NOT NULL,
    url                TEXT NOT NULL,
    signing_secret_enc BLOB NOT NULL,
    event_types        TEXT NOT NULL,
    enabled            INTEGER NOT NULL,
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL
);

CREATE INDEX webhook_endpoints_app ON webhook_endpoints (app_id);

CREATE TABLE webhook_deliveries (
    id              TEXT PRIMARY KEY,
    event_id        TEXT NOT NULL,
    endpoint_id     TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending','delivered','failed')),
    attempt_count   INTEGER NOT NULL,
    next_attempt_at BIGINT,
    last_status     INTEGER,
    last_error      TEXT,
    delivered_at    BIGINT,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL,
    UNIQUE (event_id, endpoint_id)
);

CREATE INDEX webhook_deliveries_endpoint ON webhook_deliveries (endpoint_id, status, updated_at);

CREATE TABLE provider_events (
    id                  TEXT PRIMARY KEY,
    provider_account_id TEXT NOT NULL,
    provider_event_id   TEXT NOT NULL,
    headers             TEXT NOT NULL,
    body                BLOB NOT NULL,
    verified            INTEGER NOT NULL,
    received_at         BIGINT NOT NULL,
    processed_at        BIGINT,
    error               TEXT,
    UNIQUE (provider_account_id, provider_event_id)
);
