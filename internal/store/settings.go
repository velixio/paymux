package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GetOrgSetting returns the stored JSON value for an org-level setting.
func GetOrgSetting(ctx context.Context, q Queryer, orgID, key string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM org_settings WHERE org_id = ? AND key = ?", orgID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// GetAppSetting returns the stored JSON value for an app-level override.
func GetAppSetting(ctx context.Context, q Queryer, appID, key string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM app_settings WHERE app_id = ? AND key = ?", appID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetOrgSetting upserts an org value.
func SetOrgSetting(ctx context.Context, tx Tx, orgID, key, value string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO org_settings (org_id, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (org_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		orgID, key, value, ms(now))
	return err
}

// SeedOrgSetting inserts an org value only if the key has none yet.
func SeedOrgSetting(ctx context.Context, tx Tx, orgID, key, value string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO org_settings (org_id, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (org_id, key) DO NOTHING`, orgID, key, value, ms(now))
	return err
}

// SetAppSetting upserts an app override.
func SetAppSetting(ctx context.Context, tx Tx, appID, key, value string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO app_settings (app_id, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (app_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		appID, key, value, ms(now))
	return err
}

// DeleteAppSetting removes an app override ("reset to org").
func DeleteAppSetting(ctx context.Context, tx Tx, appID, key string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM app_settings WHERE app_id = ? AND key = ?", appID, key)
	return err
}
