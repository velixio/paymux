package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version int64
	name    string // file name, e.g. 0001_init.sql
	sql     string
}

// loadMigrations validates both dialect directories, requires them to match,
// and returns the migrations of the active dialect sorted by version.
func (db *DB) loadMigrations(fsys fs.FS) ([]migration, error) {
	sets := map[string][]migration{}
	for _, dir := range []string{"sqlite", "postgres"} {
		ms, err := readMigrationDir(fsys, dir)
		if err != nil {
			return nil, err
		}
		sets[dir] = ms
	}
	if err := compareSets(sets["sqlite"], sets["postgres"]); err != nil {
		return nil, err
	}
	return sets[db.d.Name()], nil
}

func readMigrationDir(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations %s: %w", dir, err)
	}
	var out []migration
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrations/%s/%s: name must match NNNN_name.sql", dir, e.Name())
		}
		v, _ := strconv.ParseInt(m[1], 10, 64)
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations/%s: duplicate version %04d (%s and %s)", dir, v, prev, e.Name())
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, dir+"/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migrations/%s/%s: %w", dir, e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func compareSets(a, b []migration) error {
	names := func(ms []migration) map[string]bool {
		m := map[string]bool{}
		for _, x := range ms {
			m[x.name] = true
		}
		return m
	}
	an, bn := names(a), names(b)
	var diff []string
	for n := range an {
		if !bn[n] {
			diff = append(diff, "sqlite/"+n+" has no postgres counterpart")
		}
	}
	for n := range bn {
		if !an[n] {
			diff = append(diff, "postgres/"+n+" has no sqlite counterpart")
		}
	}
	if len(diff) == 0 {
		return nil
	}
	sort.Strings(diff)
	return fmt.Errorf("migration sets differ: %v", diff)
}

func (db *DB) ensureMigrationsTable(ctx context.Context) error {
	return db.Write(ctx, func(tx Tx) error {
		_, err := tx.tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`)
		return err
	})
}

func (db *DB) appliedVersions(ctx context.Context) (map[int64]bool, error) {
	if err := db.ensureMigrationsTable(ctx); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	applied := map[int64]bool{}
	err := db.Read(ctx, func(q Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT version FROM schema_migrations")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				return err
			}
			applied[v] = true
		}
		return rows.Err()
	})
	return applied, err
}

func pending(ms []migration, applied map[int64]bool) []migration {
	var out []migration
	for _, m := range ms {
		if !applied[m.version] {
			out = append(out, m)
		}
	}
	return out
}

// PendingMigrations returns how many migrations in fsys are not yet applied.
func (db *DB) PendingMigrations(ctx context.Context, fsys fs.FS) (int, error) {
	ms, err := db.loadMigrations(fsys)
	if err != nil {
		return 0, err
	}
	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	return len(pending(ms, applied)), nil
}

// Migrate applies pending migrations in order, each in its own transaction.
// On SQLite a VACUUM INTO snapshot is taken first when the database already
// has applied migrations.
func (db *DB) Migrate(ctx context.Context, fsys fs.FS) error {
	ms, err := db.loadMigrations(fsys)
	if err != nil {
		return err
	}
	applied, err := db.appliedVersions(ctx)
	if err != nil {
		return err
	}
	todo := pending(ms, applied)
	if len(todo) == 0 {
		return nil
	}
	if db.d.SupportsSnapshot() && len(applied) > 0 {
		if err := db.snapshot(ctx, todo[0].version); err != nil {
			return fmt.Errorf("pre-migration snapshot: %w", err)
		}
	}
	for _, m := range todo {
		if err := db.applyOne(ctx, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

func (db *DB) applyOne(ctx context.Context, m migration) error {
	return db.Write(ctx, func(tx Tx) error {
		if db.d.Name() == "postgres" {
			// Serialise concurrent migrators across instances.
			if _, err := tx.tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(727274)"); err != nil {
				return err
			}
		}
		var n int
		if err := tx.tx.QueryRowContext(ctx, db.d.Rebind("SELECT COUNT(*) FROM schema_migrations WHERE version = ?"), m.version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil // applied by another instance meanwhile
		}
		if _, err := tx.tx.ExecContext(ctx, m.sql); err != nil {
			return err
		}
		_, err := tx.tx.ExecContext(ctx, db.d.Rebind("INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)"),
			m.version, m.name, time.Now().UnixMilli())
		return err
	})
}

// snapshot writes <dir>/backups/pre-<version>-<unixms>.db using VACUUM INTO.
func (db *DB) snapshot(ctx context.Context, version int64) error {
	if db.path == "" {
		return errors.New("no database path for snapshot")
	}
	dir := filepath.Join(filepath.Dir(db.path), "backups")
	if err := mkdir0700(dir); err != nil {
		return err
	}
	dest := filepath.Join(dir, fmt.Sprintf("pre-%04d-%d.db", version, time.Now().UnixMilli()))
	if err := db.vacuumInto(ctx, dest); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	return nil
}
