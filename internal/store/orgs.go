package store

import (
	"context"
	"time"
)

// DefaultOrgID is the id of the single org seeded on first start (Path 1).
const DefaultOrgID = "org_default"

// SeedOrg inserts the default org if it does not exist yet. It is idempotent.
func SeedOrg(ctx context.Context, tx Tx, name string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO orgs (id, name, created_at) VALUES (?, ?, ?)
		ON CONFLICT (id) DO NOTHING`, DefaultOrgID, name, ms(now))
	return err
}

// RenameOrg sets an org's display name; ErrNotFound if there is no such org.
func RenameOrg(ctx context.Context, tx Tx, orgID, name string) error {
	res, err := tx.ExecContext(ctx, `UPDATE orgs SET name = ? WHERE id = ?`, name, orgID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// FirstOrgID returns the id of the oldest org.
func FirstOrgID(ctx context.Context, q Queryer) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, "SELECT id FROM orgs ORDER BY created_at, id LIMIT 1").Scan(&id)
	return id, notFound(err)
}
