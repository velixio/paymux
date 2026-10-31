CREATE TABLE webhook_delivery_attempts (
    id              TEXT PRIMARY KEY,
    delivery_id     TEXT NOT NULL,
    attempt_no      INTEGER NOT NULL,
    request_body    TEXT NOT NULL,
    response_status INTEGER,
    response_body   TEXT,
    error           TEXT,
    created_at      BIGINT NOT NULL,
    FOREIGN KEY (delivery_id) REFERENCES webhook_deliveries(id)
);
CREATE INDEX webhook_delivery_attempts_delivery ON webhook_delivery_attempts (delivery_id, attempt_no);
