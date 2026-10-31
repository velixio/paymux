ALTER TABLE notification_channels
ADD COLUMN telegram_delete_after_hours INTEGER NOT NULL DEFAULT 47
CHECK (telegram_delete_after_hours >= 0 AND telegram_delete_after_hours <= 47);
