-- A payment keeps the routing restrictions it was created with (the connectors
-- the app allows, and the buyer country routing rules can match on), so that
-- the pay page, a retry and a fallback route exactly as the creation did.
ALTER TABLE payments ADD COLUMN allowed_connectors TEXT NOT NULL DEFAULT '[]';
ALTER TABLE payments ADD COLUMN country TEXT NOT NULL DEFAULT '';
