package store

import (
	"context"
	"time"
)

// AuditRow is a row of audit_log. Before and After are already-redacted JSON.
type AuditRow struct {
	ID, OrgID, ActorType, ActorID, Action, Target string
	Before, After, IP                             string
	At                                            time.Time
}

// InsertAudit appends an audit row.
func InsertAudit(ctx context.Context, tx Tx, r AuditRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_log (id, org_id, actor_type, actor_id, action, target, before, after, ip, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.OrgID, r.ActorType, nullIfEmpty(r.ActorID), r.Action,
		nullIfEmpty(r.Target), nullIfEmpty(r.Before), nullIfEmpty(r.After), nullIfEmpty(r.IP), ms(r.At))
	return err
}

// ListAudit returns the newest audit rows of an org first, optionally
// restricted to one action.
func ListAudit(ctx context.Context, q Queryer, orgID, action string, limit int) ([]AuditRow, error) {
	query := `SELECT id, org_id, actor_type, COALESCE(actor_id,''), action, COALESCE(target,''),
		COALESCE(before,''), COALESCE(after,''), COALESCE(ip,''), at FROM audit_log WHERE org_id = ?`
	args := []any{orgID}
	if action != "" {
		query += " AND action = ?"
		args = append(args, action)
	}
	query += " ORDER BY at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var at int64
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ActorType, &r.ActorID, &r.Action, &r.Target, &r.Before, &r.After, &r.IP, &at); err != nil {
			return nil, err
		}
		r.At = fromMS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}
