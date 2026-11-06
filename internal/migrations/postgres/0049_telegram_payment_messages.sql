-- The Telegram message that shows a payment, one per chat. locked_until keeps
-- two notification jobs of a payment from posting or editing at the same time;
-- message_id stays 0 until the first message is posted.
CREATE TABLE telegram_payment_messages (
    org_id        TEXT NOT NULL,
    mode          TEXT NOT NULL CHECK (mode IN ('test','live')),
    chat_id       TEXT NOT NULL,
    payment_id    TEXT NOT NULL,
    message_id    BIGINT NOT NULL DEFAULT 0,
    locked_until  BIGINT NOT NULL DEFAULT 0,
    created_at    BIGINT NOT NULL,
    PRIMARY KEY (org_id, mode, chat_id, payment_id)
);
