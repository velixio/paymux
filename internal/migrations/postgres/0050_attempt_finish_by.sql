-- When a started attempt stops holding its payment open (the attempt_ttl
-- setting at the time it was made). NULL: an attempt made before the setting,
-- or a renewal, which keeps the 72h polling window.
ALTER TABLE payment_attempts ADD COLUMN finish_by BIGINT;
