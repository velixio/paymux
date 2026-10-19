package store_test

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
)

// fixtureVersion is the migration the upgrade fixtures were made at. It never
// changes: the fixtures are frozen pictures of a database as an older release
// left it, and the upgrade test proves the current migrations carry such a
// database to the present. To pin a newer baseline, add one more pair of
// fixtures; do not regenerate these.
const fixtureVersion = 5

// migrationsUpTo is the embedded migrations limited to versions <= max.
func migrationsUpTo(t *testing.T, max int) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	for _, dir := range []string{"sqlite", "postgres"} {
		entries, err := fs.ReadDir(migrations.FS, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			v, err := strconv.Atoi(e.Name()[:4])
			if err != nil || v > max {
				continue
			}
			b, err := fs.ReadFile(migrations.FS, dir+"/"+e.Name())
			if err != nil {
				t.Fatal(err)
			}
			out[dir+"/"+e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	return out
}

func readFixtureSQL(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "upgrade", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestGenerateUpgradeFixtures builds the frozen fixtures. It is skipped
// unless asked for and is kept so the fixtures can be audited and rebuilt;
// see docs/upgrades.md for the steps (the PostgreSQL dump is taken with
// pg_dump from a database this test fills).
//
//	PAYMUX_UPGRADE_FIXTURE=sqlite go test ./internal/store -run GenerateUpgradeFixtures
//	PAYMUX_UPGRADE_FIXTURE_PG_URL=postgres://... go test ./internal/store -run GenerateUpgradeFixtures
func TestGenerateUpgradeFixtures(t *testing.T) {
	ctx := t.Context()
	if os.Getenv("PAYMUX_UPGRADE_FIXTURE") == "sqlite" {
		path := filepath.Join("testdata", "upgrade", "v0005.sqlite")
		_ = os.Remove(path)
		db, err := store.Open(ctx, "sqlite://"+path, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(ctx, migrationsUpTo(t, fixtureVersion)); err != nil {
			t.Fatal(err)
		}
		db.Close()
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		for _, f := range []string{"data_common.sql", "data_sqlite.sql"} {
			if _, err := raw.ExecContext(ctx, readFixtureSQL(t, f)); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		}
		// One self-contained file: no write-ahead log left behind.
		if _, err := raw.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	if url := os.Getenv("PAYMUX_UPGRADE_FIXTURE_PG_URL"); url != "" {
		db, err := store.Open(ctx, url, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.Migrate(ctx, migrationsUpTo(t, fixtureVersion)); err != nil {
			t.Fatal(err)
		}
		raw, err := sql.Open("pgx", url)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		for _, f := range []string{"data_common.sql", "data_postgres.sql"} {
			if _, err := raw.ExecContext(ctx, readFixtureSQL(t, f)); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		}
		t.Log("filled; now take the dump with pg_dump (docs/upgrades.md)")
		return
	}
	t.Skip("set PAYMUX_UPGRADE_FIXTURE=sqlite or PAYMUX_UPGRADE_FIXTURE_PG_URL to rebuild the upgrade fixtures")
}
