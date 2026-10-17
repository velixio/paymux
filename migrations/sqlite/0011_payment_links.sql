-- Payment links: a shareable URL (/l/<slug>) that creates a normal payment for
-- whoever opens it. Money is int64 minor units; times are unix ms UTC; JSON is TEXT.

CREATE TABLE payment_links (
    id                 TEXT PRIMARY KEY,
    org_id             TEXT NOT NULL,
    mode               TEXT NOT NULL CHECK (mode IN ('test','live')),
    app_id             TEXT NOT NULL,
    slug               TEXT NOT NULL,
    -- A fixed amount, or amount = 0 with min_amount > 0 when the customer chooses
    -- (max_amount 0: no upper bound beyond the global one).
    amount             BIGINT NOT NULL DEFAULT 0,
    min_amount         BIGINT NOT NULL DEFAULT 0,
    max_amount         BIGINT NOT NULL DEFAULT 0,
    currency           TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    return_url         TEXT NOT NULL DEFAULT '',
    single_use         INTEGER NOT NULL DEFAULT 0,
    expires_at         BIGINT,
    metadata           TEXT NOT NULL DEFAULT '{}',
    active             INTEGER NOT NULL DEFAULT 1,
    deactivated_at     BIGINT,
    deactivated_reason TEXT NOT NULL DEFAULT '',
    -- A single-use link is held by one visitor while their payment is open.
    claim_hash         TEXT NOT NULL DEFAULT '',
    claim_until        BIGINT NOT NULL DEFAULT 0,
    version            BIGINT NOT NULL DEFAULT 1,
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL,
    CHECK ((amount > 0 AND min_amount = 0 AND max_amount = 0) OR (amount = 0 AND min_amount > 0))
);
CREATE UNIQUE INDEX payment_links_slug ON payment_links (slug);
CREATE INDEX payment_links_app ON payment_links (app_id, mode, id);
CREATE INDEX payment_links_org ON payment_links (org_id, id);
