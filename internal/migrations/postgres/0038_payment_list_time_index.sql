DROP INDEX payments_app_list;
CREATE INDEX payments_app_list ON payments (app_id, mode, created_at DESC, id DESC);
CREATE INDEX payments_org_list ON payments (org_id, mode, created_at DESC, id DESC);
