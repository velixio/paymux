-- checkout_ips are the IPs that opened a payment's pay page or started its
-- browser checkout. A success paid from an IP that never did, in another
-- country, goes to review (the review_ip_mismatch setting).
CREATE TABLE checkout_ips (
    payment_id TEXT NOT NULL REFERENCES payments(id),
    ip TEXT NOT NULL,
    first_seen BIGINT NOT NULL,
    last_seen BIGINT NOT NULL,
    PRIMARY KEY (payment_id, ip)
);

-- ip_lookups caches what the IP lookup service said about an IP (iprep.Info as
-- JSON), so each IP is looked up at most once a month.
CREATE TABLE ip_lookups (
    ip TEXT PRIMARY KEY,
    info TEXT NOT NULL,
    checked_at BIGINT NOT NULL
);
