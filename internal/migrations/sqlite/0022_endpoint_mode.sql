-- A webhook endpoint belongs to one mode. Until now an app's endpoints received
-- the events of both its test and its live payments, so a test payment could
-- reach a production receiver. New endpoints default to live; an existing
-- endpoint of an app that never went live (no live API key, no live payment)
-- becomes a test endpoint, which is what it was in practice.
ALTER TABLE webhook_endpoints ADD COLUMN mode TEXT NOT NULL DEFAULT 'live';

UPDATE webhook_endpoints SET mode = 'test'
 WHERE app_id NOT IN (SELECT app_id FROM api_keys WHERE mode = 'live')
   AND app_id NOT IN (SELECT app_id FROM payments WHERE mode = 'live');
