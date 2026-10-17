package store

import (
	"context"
	"time"
)

// App is a row of apps.
type App struct {
	ID, OrgID, Name, Slug, DefaultReturnURL string
	CreatedAt                               time.Time
}

// APIKey is a row of api_keys. Hash is the lowercase hex SHA-256 of the key.
type APIKey struct {
	ID, OrgID, AppID, Mode, Prefix, Hash string
	LastUsedAt                           time.Time
	RevokedAt                            time.Time
	CreatedAt                            time.Time
}

// InsertApp inserts a. A duplicate (org, slug) returns ErrConflict.
func InsertApp(ctx context.Context, tx Tx, a App) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO apps (id, org_id, name, slug, default_return_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.ID, a.OrgID, a.Name, a.Slug, nullIfEmpty(a.DefaultReturnURL), ms(a.CreatedAt), ms(a.CreatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

const appCols = "id, org_id, name, slug, COALESCE(default_return_url, ''), created_at"

func scanApp(row interface{ Scan(...any) error }) (App, error) {
	var a App
	var created int64
	if err := row.Scan(&a.ID, &a.OrgID, &a.Name, &a.Slug, &a.DefaultReturnURL, &created); err != nil {
		return App{}, notFound(err)
	}
	a.CreatedAt = fromMS(created)
	return a, nil
}

// GetApp loads an app by id.
func GetApp(ctx context.Context, q Queryer, id string) (App, error) {
	return scanApp(q.QueryRowContext(ctx, "SELECT "+appCols+" FROM apps WHERE id = ?", id))
}

// GetAppBySlug loads an app by (org, slug).
func GetAppBySlug(ctx context.Context, q Queryer, orgID, slug string) (App, error) {
	return scanApp(q.QueryRowContext(ctx, "SELECT "+appCols+" FROM apps WHERE org_id = ? AND slug = ?", orgID, slug))
}

// InsertAPIKey inserts k.
func InsertAPIKey(ctx context.Context, tx Tx, k APIKey) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO api_keys (id, org_id, app_id, mode, prefix, sha256_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, k.ID, k.OrgID, k.AppID, k.Mode, k.Prefix, k.Hash, ms(k.CreatedAt))
	return err
}

// GetAPIKeyByPrefix loads the key with the given lookup prefix.
func GetAPIKeyByPrefix(ctx context.Context, q Queryer, prefix string) (APIKey, error) {
	var k APIKey
	var used, revoked *int64
	var created int64
	err := q.QueryRowContext(ctx, `SELECT id, org_id, app_id, mode, prefix, sha256_hash, last_used_at, revoked_at, created_at
		FROM api_keys WHERE prefix = ?`, prefix).
		Scan(&k.ID, &k.OrgID, &k.AppID, &k.Mode, &k.Prefix, &k.Hash, &used, &revoked, &created)
	if err != nil {
		return APIKey{}, notFound(err)
	}
	if used != nil {
		k.LastUsedAt = fromMS(*used)
	}
	if revoked != nil {
		k.RevokedAt = fromMS(*revoked)
	}
	k.CreatedAt = fromMS(created)
	return k, nil
}

// TouchAPIKey sets last_used_at when it is unset or older than olderThan.
func TouchAPIKey(ctx context.Context, tx Tx, id string, now time.Time, olderThan time.Duration) error {
	_, err := tx.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ?
		WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`, ms(now), id, ms(now.Add(-olderThan)))
	return err
}
