CREATE INDEX payments_app_list_v2 ON payments (app_id, mode, created_at DESC, id DESC);
DROP INDEX payments_app_list;
ALTER INDEX payments_app_list_v2 RENAME TO payments_app_list;
CREATE INDEX payments_org_list ON payments (org_id, mode, created_at DESC, id DESC);
