-- A subscription keeps the routing restrictions it was created with, like a
-- payment does (0019): the connectors the caller allowed and the buyer country
-- routing rules can match on, so a later routing decision for it (a replacement
-- of its provider account or payment method) stays inside them.
ALTER TABLE subscriptions ADD COLUMN allowed_connectors TEXT NOT NULL DEFAULT '[]';
ALTER TABLE subscriptions ADD COLUMN country TEXT NOT NULL DEFAULT '';
