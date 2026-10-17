package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ErrTargetNotEmpty means the copy target already has rows.
var ErrTargetNotEmpty = errors.New("store: the target database is not empty")

// CopyOptions configures CopyAll.
type CopyOptions struct {
	// Force empties the target first instead of refusing a non-empty one.
	Force bool
	// BatchSize is the rows written per transaction (default 500).
	BatchSize int
	// Progress, when set, is called after each table with its row count.
	Progress func(table string, rows int64)
}

// TableReport is the outcome for one table.
type TableReport struct {
	Table                          string
	Rows                           int64
	SourceChecksum, TargetChecksum string
}

// CopyReport lists every table copied.
type CopyReport struct {
	Tables []TableReport
	Rows   int64
}

// CopyAll copies every table of src into dst, which may be a different
// dialect. Both must be migrated to the same version. It refuses a non-empty
// target unless Force is set (then the target is emptied first). Afterwards
// the row count and an order-independent checksum of every table are compared
// between source and target; any difference is an error. Tables are copied
// with the org first and the rest by name: the schema has no foreign keys, so
// no other order is needed.
func CopyAll(ctx context.Context, src, dst *DB, o CopyOptions) (CopyReport, error) {
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	if src == dst {
		return CopyReport{}, errors.New("store: source and target are the same database")
	}
	srcSchema, err := src.DescribeSchema(ctx)
	if err != nil {
		return CopyReport{}, fmt.Errorf("describing the source: %w", err)
	}
	dstSchema, err := dst.DescribeSchema(ctx)
	if err != nil {
		return CopyReport{}, fmt.Errorf("describing the target: %w", err)
	}
	if err := sameColumns(srcSchema, dstSchema); err != nil {
		return CopyReport{}, err
	}
	if err := sameMigrations(ctx, src, dst); err != nil {
		return CopyReport{}, err
	}
	tables := copyOrder(srcSchema)

	for _, t := range tables {
		n, err := countRows(ctx, dst, t)
		if err != nil {
			return CopyReport{}, err
		}
		if n > 0 && !o.Force {
			return CopyReport{}, fmt.Errorf("%w: table %s has %d rows (use --force to empty it first)", ErrTargetNotEmpty, t, n)
		}
	}
	if o.Force {
		for i := len(tables) - 1; i >= 0; i-- {
			t := tables[i]
			if err := dst.Write(ctx, func(tx Tx) error {
				_, err := tx.ExecContext(ctx, "DELETE FROM "+t)
				return err
			}); err != nil {
				return CopyReport{}, fmt.Errorf("emptying %s: %w", t, err)
			}
		}
	}

	var rep CopyReport
	for _, t := range tables {
		cols := srcSchema.Tables[t].Columns
		names := sortedKeys(cols)
		srcSum, rows, err := copyTable(ctx, src, dst, t, names, cols, srcSchema.Tables[t], o.BatchSize)
		if err != nil {
			return rep, fmt.Errorf("copying %s: %w", t, err)
		}
		dstSum, dstRows, err := checksumTable(ctx, dst, t, names, cols, dstSchema.Tables[t])
		if err != nil {
			return rep, fmt.Errorf("verifying %s: %w", t, err)
		}
		tr := TableReport{Table: t, Rows: rows, SourceChecksum: srcSum, TargetChecksum: dstSum}
		rep.Tables, rep.Rows = append(rep.Tables, tr), rep.Rows+rows
		if dstRows != rows || dstSum != srcSum {
			return rep, fmt.Errorf("verification failed for %s: source %d rows %s, target %d rows %s", t, rows, srcSum, dstRows, dstSum)
		}
		if o.Progress != nil {
			o.Progress(t, rows)
		}
	}
	return rep, nil
}

func sameColumns(a, b Schema) error {
	var diffs []string
	for t, ta := range a.Tables {
		tb, ok := b.Tables[t]
		if !ok {
			diffs = append(diffs, "table "+t+" is missing in the target")
			continue
		}
		if !reflect.DeepEqual(ta.Columns, tb.Columns) {
			diffs = append(diffs, "table "+t+" has different columns")
		}
	}
	for t := range b.Tables {
		if _, ok := a.Tables[t]; !ok {
			diffs = append(diffs, "table "+t+" is missing in the source")
		}
	}
	if len(diffs) == 0 {
		return nil
	}
	sort.Strings(diffs)
	return fmt.Errorf("source and target schemas differ (migrate both to the same version): %s", strings.Join(diffs, "; "))
}

func sameMigrations(ctx context.Context, a, b *DB) error {
	va, err := appliedList(ctx, a)
	if err != nil {
		return err
	}
	vb, err := appliedList(ctx, b)
	if err != nil {
		return err
	}
	if !slices.Equal(va, vb) {
		return fmt.Errorf("source and target are at different migration versions (source %v, target %v)", va, vb)
	}
	return nil
}

func appliedList(ctx context.Context, db *DB) ([]int64, error) {
	m, err := db.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	slices.Sort(out)
	return out, nil
}

func copyOrder(s Schema) []string {
	names := make([]string, 0, len(s.Tables))
	for t := range s.Tables {
		if t != "orgs" {
			names = append(names, t)
		}
	}
	sort.Strings(names)
	if _, ok := s.Tables["orgs"]; ok {
		names = append([]string{"orgs"}, names...)
	}
	return names
}

func sortedKeys(m map[string]Column) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func countRows(ctx context.Context, db *DB, table string) (int64, error) {
	var n int64
	err := db.Read(ctx, func(q Querier) error {
		return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n)
	})
	return n, err
}

func selectAll(names []string, table string, t Table) string {
	q := "SELECT " + strings.Join(names, ", ") + " FROM " + table
	order := names
	if pk, ok := t.Indexes["primary"]; ok && len(pk.Columns) > 0 {
		order = pk.Columns
	}
	return q + " ORDER BY " + strings.Join(order, ", ")
}

// copyTable streams the table from src into dst in batches and returns the
// source checksum and row count.
func copyTable(ctx context.Context, src, dst *DB, table string, names []string, cols map[string]Column, t Table, batch int) (string, int64, error) {
	insert := "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + ")"
	var pending [][]any
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		rows := pending
		pending = nil
		return dst.Write(ctx, func(tx Tx) error {
			for _, r := range rows {
				if _, err := tx.ExecContext(ctx, insert, r...); err != nil {
					return err
				}
			}
			return nil
		})
	}
	var sum checksum
	var n int64
	err := src.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, selectAll(names, table, t))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals := make([]any, len(names))
			ptrs := make([]any, len(names))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			for i, name := range names {
				vals[i] = normalize(vals[i], cols[name])
			}
			sum.add(names, cols, vals)
			pending = append(pending, vals)
			n++
			if len(pending) >= batch {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		return "", n, err
	}
	if err := flush(); err != nil {
		return "", n, err
	}
	return sum.String(n), n, nil
}

func checksumTable(ctx context.Context, db *DB, table string, names []string, cols map[string]Column, t Table) (string, int64, error) {
	var sum checksum
	var n int64
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, selectAll(names, table, t))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals := make([]any, len(names))
			ptrs := make([]any, len(names))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			for i, name := range names {
				vals[i] = normalize(vals[i], cols[name])
			}
			sum.add(names, cols, vals)
			n++
		}
		return rows.Err()
	})
	return sum.String(n), n, err
}

// normalize maps a scanned value to the Go type of its column kind, so a
// value reads the same from either dialect.
func normalize(v any, c Column) any {
	switch c.Type {
	case "int":
		switch x := v.(type) {
		case int64:
			return x
		case int32:
			return int64(x)
		case int:
			return int64(x)
		case []byte:
			n, _ := strconv.ParseInt(string(x), 10, 64)
			return n
		case string:
			n, _ := strconv.ParseInt(x, 10, 64)
			return n
		}
	case "text":
		switch x := v.(type) {
		case []byte:
			return string(x)
		}
	case "bytes":
		if x, ok := v.(string); ok {
			return []byte(x)
		}
	}
	return v
}

// checksum is an order-independent digest of a table: the sum, modulo 2^64,
// of a hash of every row.
type checksum struct{ sum uint64 }

func (c *checksum) add(names []string, cols map[string]Column, vals []any) {
	h := sha256.New()
	var lenBuf [8]byte
	for i, name := range names {
		h.Write([]byte(name))
		switch v := vals[i].(type) {
		case nil:
			h.Write([]byte{'N'})
		case int64:
			h.Write([]byte{'I'})
			binary.BigEndian.PutUint64(lenBuf[:], uint64(v))
			h.Write(lenBuf[:])
		case string:
			h.Write([]byte{'T'})
			binary.BigEndian.PutUint64(lenBuf[:], uint64(len(v)))
			h.Write(lenBuf[:])
			h.Write([]byte(v))
		case []byte:
			h.Write([]byte{'B'})
			binary.BigEndian.PutUint64(lenBuf[:], uint64(len(v)))
			h.Write(lenBuf[:])
			h.Write(v)
		default:
			h.Write([]byte(fmt.Sprintf("?%v", v)))
		}
	}
	c.sum += binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

func (c checksum) String(rows int64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], c.sum)
	return hex.EncodeToString(b[:]) + ":" + strconv.FormatInt(rows, 10)
}
