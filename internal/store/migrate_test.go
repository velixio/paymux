package store_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
)

func mapFS(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// both puts the same body under sqlite/ and postgres/.
func both(files map[string]string) fs.FS {
	out := map[string]string{}
	for name, body := range files {
		out["sqlite/"+name] = body
		out["postgres/"+name] = body
	}
	return mapFS(out)
}

func maxVersion(t *testing.T, db *store.DB) int {
	t.Helper()
	var v int
	err := db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, db *store.DB, name string) bool {
	t.Helper()
	s, err := db.DescribeSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := s.Tables[name]
	return ok
}

var twoMigrations = map[string]string{
	"0001_a.sql": "CREATE TABLE a (id TEXT PRIMARY KEY);",
	"0002_b.sql": "CREATE TABLE b (id TEXT PRIMARY KEY, a_id TEXT NOT NULL);\nCREATE INDEX b_a ON b (a_id);",
}

func TestMigrateFreshAndIdempotent(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		fsys := both(twoMigrations)
		n, err := db.PendingMigrations(ctx, fsys)
		if err != nil || n != 2 {
			t.Fatalf("pending=%d err=%v", n, err)
		}
		if err := db.Migrate(ctx, fsys); err != nil {
			t.Fatal(err)
		}
		if v := maxVersion(t, db); v != 2 {
			t.Fatalf("max=%d", v)
		}
		if n, _ := db.PendingMigrations(ctx, fsys); n != 0 {
			t.Fatalf("pending after migrate=%d", n)
		}
		if err := db.Migrate(ctx, fsys); err != nil {
			t.Fatalf("second migrate: %v", err)
		}
		if v := maxVersion(t, db); v != 2 {
			t.Fatalf("max=%d", v)
		}
		if !tableExists(t, db, "a") || !tableExists(t, db, "b") {
			t.Fatal("tables missing")
		}
	})
}

func TestFailedMigrationRollsBack(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		files := map[string]string{}
		for k, v := range twoMigrations {
			files[k] = v
		}
		files["0003_bad.sql"] = "CREATE TABLE x (id TEXT PRIMARY KEY);\nTHIS IS NOT SQL;"
		err := db.Migrate(ctx, both(files))
		if err == nil || !strings.Contains(err.Error(), "0003_") {
			t.Fatalf("want error naming 0003_, got %v", err)
		}
		if v := maxVersion(t, db); v != 2 {
			t.Fatalf("max=%d, want 2", v)
		}
		if tableExists(t, db, "x") {
			t.Fatal("table x must not exist")
		}
	})
}

func TestMigrationFilenameValidation(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		cases := map[string]map[string]string{
			"short version": {"1_x.sql": "SELECT 1;"},
			"dash":          {"0002-x.sql": "SELECT 1;"},
			"duplicate":     {"0001_a.sql": "SELECT 1;", "0001_b.sql": "SELECT 1;"},
		}
		for name, files := range cases {
			if err := db.Migrate(ctx, both(files)); err == nil {
				t.Errorf("%s: expected error", name)
			}
		}
	})
}

func TestDialectDirsMustMatch(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		cases := map[string]fs.FS{
			"missing in postgres": mapFS(map[string]string{
				"sqlite/0001_a.sql": "SELECT 1;", "sqlite/0002_b.sql": "SELECT 1;", "postgres/0001_a.sql": "SELECT 1;"}),
			"name mismatch": mapFS(map[string]string{
				"sqlite/0001_a.sql": "SELECT 1;", "postgres/0001_z.sql": "SELECT 1;"}),
		}
		for name, fsys := range cases {
			err := db.Migrate(ctx, fsys)
			if err == nil || !strings.Contains(err.Error(), "migration sets differ:") {
				t.Errorf("%s: got %v", name, err)
			}
		}
	})
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "backups", "pre-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSnapshotBeforeMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := openSQLite(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	one := both(map[string]string{"0001_a.sql": twoMigrations["0001_a.sql"]})
	if err := db.Migrate(ctx, one); err != nil {
		t.Fatal(err)
	}
	if got := backups(t, dir); len(got) != 0 {
		t.Fatalf("fresh DB must not snapshot, got %v", got)
	}
	if err := db.Migrate(ctx, both(twoMigrations)); err != nil {
		t.Fatal(err)
	}
	got := backups(t, dir)
	if len(got) != 1 || !strings.Contains(filepath.Base(got[0]), "pre-0002-") {
		t.Fatalf("backups=%v", got)
	}
	if st, err := os.Stat(got[0]); err != nil || st.Size() == 0 {
		t.Fatalf("snapshot empty or missing: %v", err)
	}
}

func TestSnapshotFailureAbortsMigration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	db, err := openSQLite(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, both(map[string]string{"0001_a.sql": twoMigrations["0001_a.sql"]})); err != nil {
		t.Fatal(err)
	}
	bdir := filepath.Join(dir, "backups")
	if err := os.Mkdir(bdir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bdir, 0o700) })
	if err := db.Migrate(ctx, both(twoMigrations)); err == nil {
		t.Fatal("expected snapshot error")
	}
	if v := maxVersion(t, db); v != 1 {
		t.Fatalf("max=%d, want 1", v)
	}
	if tableExists(t, db, "b") {
		t.Fatal("0002 must not have been applied")
	}
}

func TestRealMigrationsApply(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		for _, tbl := range []string{"orgs", "jobs", "schedules"} {
			if !tableExists(t, db, tbl) {
				t.Errorf("missing table %s", tbl)
			}
		}
	})
}

func TestSchemaParity(t *testing.T) {
	sq, pg := storetest.SQLite(t), storetest.Postgres(t)
	for _, db := range []*store.DB{sq, pg} {
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
	}
	a, err := sq.DescribeSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pg.DescribeSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffSchema(a, b); d != "" {
		t.Fatalf("schema parity broken (left=sqlite, right=postgres):\n%s", d)
	}
}

func diffSchema(a, b store.Schema) string {
	var out []string
	for name, ta := range a.Tables {
		tb, ok := b.Tables[name]
		if !ok {
			out = append(out, fmt.Sprintf("table %s: only in sqlite", name))
			continue
		}
		for c, ca := range ta.Columns {
			if cb, ok := tb.Columns[c]; !ok {
				out = append(out, fmt.Sprintf("table %s column %s: only in sqlite", name, c))
			} else if ca != cb {
				out = append(out, fmt.Sprintf("table %s column %s: sqlite %+v vs postgres %+v", name, c, ca, cb))
			}
		}
		for c := range tb.Columns {
			if _, ok := ta.Columns[c]; !ok {
				out = append(out, fmt.Sprintf("table %s column %s: only in postgres", name, c))
			}
		}
		for i, ia := range ta.Indexes {
			if ib, ok := tb.Indexes[i]; !ok {
				out = append(out, fmt.Sprintf("table %s index %s: only in sqlite", name, i))
			} else if fmt.Sprint(ia) != fmt.Sprint(ib) {
				out = append(out, fmt.Sprintf("table %s index %s: sqlite %+v vs postgres %+v", name, i, ia, ib))
			}
		}
		for i := range tb.Indexes {
			if _, ok := ta.Indexes[i]; !ok {
				out = append(out, fmt.Sprintf("table %s index %s: only in postgres", name, i))
			}
		}
	}
	for name := range b.Tables {
		if _, ok := a.Tables[name]; !ok {
			out = append(out, fmt.Sprintf("table %s: only in postgres", name))
		}
	}
	return strings.Join(out, "\n")
}
