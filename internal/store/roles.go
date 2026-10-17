package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Errors of the roles table.
var (
	// ErrUnknownRole means a user was given a role that does not exist.
	ErrUnknownRole = errors.New("unknown role")
	// ErrRoleInUse means a role cannot be deleted while users have it.
	ErrRoleInUse = errors.New("role is assigned to users")
	// ErrBuiltinRole means the operation is not allowed on a built-in role.
	ErrBuiltinRole = errors.New("built-in role")
)

// BuiltinRoleNames are the roles defined in code. They are always valid for a
// user, whether or not their rows have been listed in the roles table yet.
var BuiltinRoleNames = []string{"admin", "operator", "viewer"}

// RoleRow is a row of roles: a name and the authz actions it grants.
type RoleRow struct {
	Name                 string
	Builtin              bool
	Actions              []string
	CreatedAt, UpdatedAt time.Time
}

// roleKnown reports whether name is a built-in or a stored role.
func roleKnown(ctx context.Context, q Queryer, name string) (bool, error) {
	for _, b := range BuiltinRoleNames {
		if b == name {
			return true, nil
		}
	}
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM roles WHERE name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// RoleExists reports whether name is a built-in or stored role.
func (db *DB) RoleExists(ctx context.Context, name string) (bool, error) {
	var ok bool
	err := db.Read(ctx, func(q Querier) (err error) { ok, err = roleKnown(ctx, q, name); return })
	return ok, err
}

// ListRoles returns every stored role, built-ins first, then by name.
func (db *DB) ListRoles(ctx context.Context) ([]RoleRow, error) {
	var out []RoleRow
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT name, builtin, actions, created_at, updated_at FROM roles ORDER BY builtin DESC, name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RoleRow
			var builtin int
			var actions string
			var created, updated int64
			if err := rows.Scan(&r.Name, &builtin, &actions, &created, &updated); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(actions), &r.Actions); err != nil {
				return fmt.Errorf("role %s: bad actions: %w", r.Name, err)
			}
			r.Builtin, r.CreatedAt, r.UpdatedAt = builtin != 0, fromMS(created), fromMS(updated)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// SeedBuiltinRoles writes the built-in roles, replacing their stored actions
// with the ones in code. It is idempotent; it never touches custom roles.
func (db *DB) SeedBuiltinRoles(ctx context.Context, roles []RoleRow, now time.Time) error {
	return db.Write(ctx, func(tx Tx) error {
		for _, r := range roles {
			actions, _ := json.Marshal(nonNilStrings(r.Actions))
			if _, err := tx.ExecContext(ctx, `INSERT INTO roles (name, builtin, actions, created_at, updated_at) VALUES (?, 1, ?, ?, ?)
				ON CONFLICT (name) DO UPDATE SET builtin = 1, actions = excluded.actions, updated_at = excluded.updated_at`,
				r.Name, string(actions), ms(now), ms(now)); err != nil {
				return err
			}
		}
		return nil
	})
}

// CreateRole creates a custom role. An existing name (built-in or not) is
// ErrDuplicate or ErrBuiltinRole, and nothing is changed.
func (db *DB) CreateRole(ctx context.Context, name string, actions []string, now time.Time) error {
	for _, b := range BuiltinRoleNames {
		if b == name {
			return ErrBuiltinRole
		}
	}
	enc, _ := json.Marshal(nonNilStrings(actions))
	return db.Write(ctx, func(tx Tx) error {
		var one int
		switch e := tx.QueryRowContext(ctx, `SELECT 1 FROM roles WHERE name = ?`, name).Scan(&one); {
		case e == nil:
			return ErrDuplicate
		case !errors.Is(e, sql.ErrNoRows):
			return e
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO roles (name, builtin, actions, created_at, updated_at) VALUES (?, 0, ?, ?, ?)`,
			name, string(enc), ms(now), ms(now))
		return err
	})
}

// SaveRole creates a custom role or replaces its actions. A built-in name is
// ErrBuiltinRole.
func (db *DB) SaveRole(ctx context.Context, name string, actions []string, now time.Time) (created bool, err error) {
	for _, b := range BuiltinRoleNames {
		if b == name {
			return false, ErrBuiltinRole
		}
	}
	enc, _ := json.Marshal(nonNilStrings(actions))
	err = db.Write(ctx, func(tx Tx) error {
		var builtin int
		switch e := tx.QueryRowContext(ctx, `SELECT builtin FROM roles WHERE name = ?`, name).Scan(&builtin); {
		case errors.Is(e, sql.ErrNoRows):
			created = true
			_, e = tx.ExecContext(ctx, `INSERT INTO roles (name, builtin, actions, created_at, updated_at) VALUES (?, 0, ?, ?, ?)`,
				name, string(enc), ms(now), ms(now))
			return e
		case e != nil:
			return e
		case builtin != 0:
			return ErrBuiltinRole
		}
		_, e := tx.ExecContext(ctx, `UPDATE roles SET actions = ?, updated_at = ? WHERE name = ?`, string(enc), ms(now), name)
		return e
	})
	return created, err
}

// DeleteRole removes a custom role. It is ErrNotFound when there is none,
// ErrBuiltinRole for a built-in and ErrRoleInUse while a user has it.
func (db *DB) DeleteRole(ctx context.Context, name string) error {
	for _, b := range BuiltinRoleNames {
		if b == name {
			return ErrBuiltinRole
		}
	}
	return db.Write(ctx, func(tx Tx) error {
		var users int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ?`, name).Scan(&users); err != nil {
			return err
		}
		if users > 0 {
			return ErrRoleInUse
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM roles WHERE name = ? AND builtin = 0`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RoleUserCounts returns the number of users per role name.
func (db *DB) RoleUserCounts(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT role, COUNT(*) FROM users GROUP BY role`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var n int
			if err := rows.Scan(&name, &n); err != nil {
				return err
			}
			out[name] = n
		}
		return rows.Err()
	})
	return out, err
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
