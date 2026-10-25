package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// SetUserTheme stores the dashboard theme a user chose for themselves; an
// empty name clears it (the organisation default applies again).
func SetUserTheme(ctx context.Context, tx Tx, userID, name string) error {
	var v any
	if name != "" {
		v = name
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET theme = ? WHERE id = ?`, v, userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserTheme returns the user's own theme, or "" when they have not chosen one.
func UserTheme(ctx context.Context, q Queryer, userID string) (string, error) {
	var name sql.NullString
	err := q.QueryRowContext(ctx, `SELECT theme FROM users WHERE id = ?`, userID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name.String, err
}

// ThemeUsers lists, by email, the users who chose the theme. Users belong to
// the installation rather than to one organisation, so org does not narrow
// the list; it is part of the signature so callers say whose themes they mean.
func ThemeUsers(ctx context.Context, q Queryer, org, name string) ([]string, error) {
	_ = org
	rows, err := q.QueryContext(ctx, `SELECT email FROM users WHERE theme = ? ORDER BY email`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RenameThemeRefs points everything that names the theme old at new: the
// organisation's default (when it is old) and the users who chose old.
func RenameThemeRefs(ctx context.Context, tx Tx, org, old, new string) error {
	was, _ := json.Marshal(old)
	now, _ := json.Marshal(new)
	if _, err := tx.ExecContext(ctx, `UPDATE org_settings SET value = ? WHERE org_id = ? AND key = 'theme' AND value = ?`,
		string(now), org, string(was)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE users SET theme = ? WHERE theme = ?`, new, old)
	return err
}
