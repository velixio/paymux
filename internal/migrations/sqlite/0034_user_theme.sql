-- A user's own dashboard theme. NULL means "use the organisation default".
ALTER TABLE users ADD COLUMN theme TEXT;
