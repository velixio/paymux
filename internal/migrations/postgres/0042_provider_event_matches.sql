CREATE TABLE provider_event_matches (
    provider_event_id TEXT NOT NULL,
    object_type       TEXT NOT NULL,
    object_id         TEXT NOT NULL,
    payment_id        TEXT,
    provider_id       TEXT,
    created_at        BIGINT NOT NULL,
    PRIMARY KEY (provider_event_id, object_type, object_id),
    FOREIGN KEY (provider_event_id) REFERENCES provider_events(id)
);
CREATE INDEX provider_event_matches_payment ON provider_event_matches (payment_id);
