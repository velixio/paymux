package store

import (
	"context"
	"fmt"
	"strings"
)

// Schema is a dialect-neutral description of the database, used by the
// parity test to prove both migration sets produce the same shape.
type Schema struct{ Tables map[string]Table }

type Table struct {
	Columns map[string]Column
	Indexes map[string]Index
}

// Column.Type is one of "int", "text", "bytes".
type Column struct {
	Type    string
	NotNull bool
}

// Index columns are in index order. Primary keys appear as index "primary";
// UNIQUE constraints as "unique:<cols>"; explicit indexes by their name.
type Index struct {
	Columns []string
	Unique  bool
	Partial bool
}

func normType(t string) string {
	t = strings.ToUpper(strings.TrimSpace(t))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "INTEGER", "INT", "BIGINT", "INT8", "INT4", "SMALLINT":
		return "int"
	case "TEXT", "VARCHAR", "CHARACTER VARYING":
		return "text"
	case "BLOB", "BYTEA":
		return "bytes"
	}
	return strings.ToLower(t)
}

// DescribeSchema returns the normalised schema, excluding schema_migrations.
func (db *DB) DescribeSchema(ctx context.Context) (Schema, error) {
	if db.d.Name() == "sqlite" {
		return db.describeSQLite(ctx)
	}
	return db.describePostgres(ctx)
}

func (s Schema) table(name string) Table {
	t, ok := s.Tables[name]
	if !ok {
		t = Table{Columns: map[string]Column{}, Indexes: map[string]Index{}}
		s.Tables[name] = t
	}
	return t
}

func (db *DB) describeSQLite(ctx context.Context) (Schema, error) {
	s := Schema{Tables: map[string]Table{}}
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master
			WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations'`)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			names = append(names, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, tn := range names {
			t := s.table(tn)
			cr, err := q.QueryContext(ctx, `SELECT name, type, "notnull", pk FROM pragma_table_info(?)`, tn)
			if err != nil {
				return err
			}
			for cr.Next() {
				var name, typ string
				var notnull, pk int
				if err := cr.Scan(&name, &typ, &notnull, &pk); err != nil {
					cr.Close()
					return err
				}
				t.Columns[name] = Column{Type: normType(typ), NotNull: notnull == 1 || pk > 0}
			}
			cr.Close()
			if err := cr.Err(); err != nil {
				return err
			}
			type idx struct {
				name, origin    string
				unique, partial bool
			}
			var idxs []idx
			ir, err := q.QueryContext(ctx, `SELECT name, origin, "unique", partial FROM pragma_index_list(?)`, tn)
			if err != nil {
				return err
			}
			for ir.Next() {
				var i idx
				var u, p int
				if err := ir.Scan(&i.name, &i.origin, &u, &p); err != nil {
					ir.Close()
					return err
				}
				i.unique, i.partial = u == 1, p == 1
				idxs = append(idxs, i)
			}
			ir.Close()
			if err := ir.Err(); err != nil {
				return err
			}
			for _, i := range idxs {
				var cols []string
				xr, err := q.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, i.name)
				if err != nil {
					return err
				}
				for xr.Next() {
					var c string
					if err := xr.Scan(&c); err != nil {
						xr.Close()
						return err
					}
					cols = append(cols, c)
				}
				xr.Close()
				if err := xr.Err(); err != nil {
					return err
				}
				t.Indexes[indexKey(i.origin, i.name, cols)] = Index{Columns: cols, Unique: i.unique, Partial: i.partial}
			}
		}
		return nil
	})
	return s, err
}

// indexKey maps a sqlite origin code ('pk','u','c') or postgres equivalent to
// a dialect-neutral key.
func indexKey(origin, name string, cols []string) string {
	switch origin {
	case "pk":
		return "primary"
	case "u":
		return "unique:" + strings.Join(cols, ",")
	}
	return name
}

func (db *DB) describePostgres(ctx context.Context) (Schema, error) {
	s := Schema{Tables: map[string]Table{}}
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, `SELECT c.table_name, c.column_name, c.data_type, c.is_nullable
			FROM information_schema.columns c
			JOIN information_schema.tables t
			  ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
			WHERE c.table_schema = current_schema() AND c.table_name <> 'schema_migrations'`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var tn, cn, dt, nullable string
			if err := rows.Scan(&tn, &cn, &dt, &nullable); err != nil {
				rows.Close()
				return err
			}
			s.table(tn).Columns[cn] = Column{Type: normType(dt), NotNull: nullable == "NO"}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		ir, err := q.QueryContext(ctx, `SELECT c.relname, i.relname, ix.indisunique, ix.indisprimary,
				(ix.indpred IS NOT NULL),
				EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = ix.indexrelid AND con.contype = 'u'),
				COALESCE((SELECT string_agg(a.attname, ',' ORDER BY k.ord)
				          FROM unnest(ix.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
				          JOIN pg_attribute a ON a.attrelid = ix.indrelid AND a.attnum = k.attnum), '')
			FROM pg_index ix
			JOIN pg_class c ON c.oid = ix.indrelid
			JOIN pg_class i ON i.oid = ix.indexrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relkind = 'r' AND c.relname <> 'schema_migrations'`)
		if err != nil {
			return err
		}
		defer ir.Close()
		for ir.Next() {
			var tn, in, cols string
			var unique, primary, partial, isUC bool
			if err := ir.Scan(&tn, &in, &unique, &primary, &partial, &isUC, &cols); err != nil {
				return err
			}
			origin := "c"
			switch {
			case primary:
				origin = "pk"
			case isUC:
				origin = "u"
			}
			cl := strings.Split(cols, ",")
			if _, ok := s.Tables[tn]; !ok {
				return fmt.Errorf("index %s on unknown table %s", in, tn)
			}
			s.Tables[tn].Indexes[indexKey(origin, in, cl)] = Index{Columns: cl, Unique: unique, Partial: partial}
		}
		return ir.Err()
	})
	return s, err
}
