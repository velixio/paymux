-- A user's own sign-in approval PIN, encrypted. NULL means sign-in needs no approval.
ALTER TABLE users ADD COLUMN signin_pin_enc BYTEA;
