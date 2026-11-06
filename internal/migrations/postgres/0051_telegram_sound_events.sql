-- The events whose Telegram messages ping the chat, as a JSON array.
ALTER TABLE notification_channels
ADD COLUMN telegram_sound_events TEXT NOT NULL DEFAULT '["payment.succeeded","refund.succeeded","dispute.opened","dispute.lost"]';
