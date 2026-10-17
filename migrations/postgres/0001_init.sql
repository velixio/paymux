CREATE TABLE orgs (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at BIGINT NOT NULL
);

CREATE TABLE jobs (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    payload      TEXT NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('pending','running','done','dead')),
    run_at       BIGINT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL,
    locked_until BIGINT,
    last_error   TEXT,
    dedupe_key   TEXT,
    created_at   BIGINT NOT NULL,
    updated_at   BIGINT NOT NULL
);

CREATE INDEX jobs_state_run_at ON jobs (state, run_at);

CREATE UNIQUE INDEX jobs_dedupe ON jobs (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND state IN ('pending','running');

CREATE TABLE schedules (
    name         TEXT PRIMARY KEY,
    next_run_at  BIGINT NOT NULL,
    locked_until BIGINT,
    updated_at   BIGINT NOT NULL
);
