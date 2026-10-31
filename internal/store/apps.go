package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// App is a row of apps.
type App struct {
	ID, OrgID, Name, Slug, DefaultReturnURL string
	CreatedAt                               time.Time
	// AcceptedCurrencies is empty when the app accepts every currency that its
	// routed provider accounts support.
	AcceptedCurrencies []string
}

// APIKey is a row of api_keys. Hash is the lowercase hex SHA-256 of the key.
type APIKey struct {
	ID, OrgID, AppID, Mode, Prefix, Hash string
	LastUsedAt                           time.Time
	RevokedAt                            time.Time
	CreatedAt                            time.Time
	// Scopes limits what the key may do; nil means every scope.
	Scopes []string
	// ExpiresAt, when set, is when the key stops working (a rotated-out key).
	ExpiresAt time.Time
}

const apiKeyCols = "id, org_id, app_id, mode, prefix, sha256_hash, last_used_at, revoked_at, created_at, scopes, expires_at"

func scanAPIKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var used, revoked, expires sql.NullInt64
	var scopes sql.NullString
	var created int64
	if err := row.Scan(&k.ID, &k.OrgID, &k.AppID, &k.Mode, &k.Prefix, &k.Hash, &used, &revoked, &created, &scopes, &expires); err != nil {
		return APIKey{}, err
	}
	k.LastUsedAt, k.RevokedAt, k.CreatedAt, k.ExpiresAt = fromNullMS(used), fromNullMS(revoked), fromMS(created), fromNullMS(expires)
	if scopes.Valid {
		if err := json.Unmarshal([]byte(scopes.String), &k.Scopes); err != nil {
			return APIKey{}, err
		}
		if k.Scopes == nil {
			k.Scopes = []string{}
		}
	}
	return k, nil
}

// InsertApp inserts a. A duplicate (org, slug) returns ErrConflict.
func InsertApp(ctx context.Context, tx Tx, a App) error {
	currencies, _ := json.Marshal(a.AcceptedCurrencies)
	_, err := tx.ExecContext(ctx, `INSERT INTO apps (id, org_id, name, slug, default_return_url, accepted_currencies, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, a.OrgID, a.Name, a.Slug, nullIfEmpty(a.DefaultReturnURL), string(currencies), ms(a.CreatedAt), ms(a.CreatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

const appCols = "id, org_id, name, slug, COALESCE(default_return_url, ''), accepted_currencies, created_at"

func scanApp(row interface{ Scan(...any) error }) (App, error) {
	var a App
	var currencies string
	var created int64
	if err := row.Scan(&a.ID, &a.OrgID, &a.Name, &a.Slug, &a.DefaultReturnURL, &currencies, &created); err != nil {
		return App{}, notFound(err)
	}
	if err := json.Unmarshal([]byte(currencies), &a.AcceptedCurrencies); err != nil {
		return App{}, err
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
	var scopes any
	if k.Scopes != nil {
		b, _ := json.Marshal(k.Scopes)
		scopes = string(b)
	}
	var expires any
	if !k.ExpiresAt.IsZero() {
		expires = ms(k.ExpiresAt)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO api_keys (id, org_id, app_id, mode, prefix, sha256_hash, created_at, scopes, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, k.ID, k.OrgID, k.AppID, k.Mode, k.Prefix, k.Hash, ms(k.CreatedAt), scopes, expires)
	return err
}

// GetAPIKeyByPrefix loads the key with the given lookup prefix.
func GetAPIKeyByPrefix(ctx context.Context, q Queryer, prefix string) (APIKey, error) {
	k, err := scanAPIKey(q.QueryRowContext(ctx, "SELECT "+apiKeyCols+" FROM api_keys WHERE prefix = ?", prefix))
	if err != nil {
		return APIKey{}, notFound(err)
	}
	return k, nil
}

// ExpireAPIKeys makes every live key of the app and mode, except exceptID, stop
// working at the given time (unless it already expires sooner). It returns the
// keys it changed.
func ExpireAPIKeys(ctx context.Context, tx Tx, appID, mode, exceptID string, at time.Time) ([]APIKey, error) {
	rows, err := tx.QueryContext(ctx, "SELECT "+apiKeyCols+` FROM api_keys WHERE app_id = ? AND mode = ? AND id <> ? AND revoked_at IS NULL
		AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at, id`, appID, mode, exceptID, ms(at))
	if err != nil {
		return nil, err
	}
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET expires_at = ? WHERE id = ?`, ms(at), out[i].ID); err != nil {
			return nil, err
		}
		out[i].ExpiresAt = at
	}
	return out, nil
}

// TouchAPIKey sets last_used_at when it is unset or older than olderThan.
func TouchAPIKey(ctx context.Context, tx Tx, id string, now time.Time, olderThan time.Duration) error {
	_, err := tx.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ?
		WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`, ms(now), id, ms(now.Add(-olderThan)))
	return err
}

// GetAppInOrg loads an app by id, but only if it belongs to orgID; any other
// org's app is ErrNotFound.
func GetAppInOrg(ctx context.Context, q Queryer, orgID, id string) (App, error) {
	return scanApp(q.QueryRowContext(ctx, "SELECT "+appCols+" FROM apps WHERE id = ? AND org_id = ?", id, orgID))
}
