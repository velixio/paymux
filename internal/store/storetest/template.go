package storetest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
)

// Applying 26 migrations costs about two seconds per database under the race
// detector, and nearly every test wants a migrated database. So each migration
// set is applied once, to a template, and a test gets a copy of it: a file copy
// on SQLite, CREATE DATABASE ... TEMPLATE on PostgreSQL. The template is named
// after a hash of the migration files, so a changed migration makes a new one.

var (
	migrationsHashOnce sync.Once
	migrationsHash     string
)

func hashOfMigrations(t testing.TB) string {
	migrationsHashOnce.Do(func() {
		h := sha256.New()
		err := fs.WalkDir(migrations.FS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(migrations.FS, p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
			h.Write(b)
			return nil
		})
		if err != nil {
			panic(err)
		}
		migrationsHash = hex.EncodeToString(h.Sum(nil))[:16]
	})
	return migrationsHash
}

// SQLiteMigrated opens a SQLite store in a temp dir that already has every
// migration applied.
func SQLiteMigrated(t *testing.T) *store.DB {
	t.Helper()
	tpl := sqliteTemplate(t)
	dir := t.TempDir()
	if err := copyFile(tpl, filepath.Join(dir, "paymux.db")); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), "", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// sqliteTemplate returns the path of a migrated database file, making it the
// first time. It lives under the system temp directory, shared by every test
// binary on the machine and by later runs; it is written aside and renamed, so
// a reader never sees a half-written file.
func sqliteTemplate(t *testing.T) string {
	t.Helper()
	root := filepath.Join(os.TempDir(), "paymux-storetest")
	final := filepath.Join(root, "sqlite-"+hashOfMigrations(t)+".db")
	if _, err := os.Stat(final); err == nil {
		return final
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	work, err := os.MkdirTemp(root, "build-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	db, err := store.Open(context.Background(), "", work, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background(), migrations.FS); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil { // the last connection closing checkpoints the WAL into the file
		t.Fatal(err)
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(work, "paymux.db"+side)); err == nil {
			t.Fatalf("the template database left a %s file behind", side)
		}
	}
	if err := os.Rename(filepath.Join(work, "paymux.db"), final); err != nil && !os.IsExist(err) {
		if _, serr := os.Stat(final); serr != nil {
			t.Fatal(err)
		}
	}
	return final
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// PostgresMigrated opens a store on a fresh database made from the migrated
// template database. Without the right to create databases it falls back to a
// fresh schema and applies the migrations.
func PostgresMigrated(t *testing.T) *store.DB {
	t.Helper()
	raw := os.Getenv("PAYMUX_TEST_POSTGRES_URL")
	if raw == "" {
		return Postgres(t) // skips or fails as the environment says
	}
	if u, ok := cloneFromTemplate(t, raw); ok {
		db, err := store.Open(context.Background(), u, t.TempDir(), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() }) // before the database is dropped
		return db
	}
	db := Postgres(t)
	if err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatal(err)
	}
	return db
}

var (
	pgMu       sync.Mutex
	pgTemplate string // database name of the template, once made by this process
	pgNoClone  bool   // creating databases is not allowed here
)

// cloneFromTemplate creates a database from the template (making the template
// first if needed) and returns its URL.
func cloneFromTemplate(t *testing.T, raw string) (string, bool) {
	hashOfMigrations(t)
	ctx := context.Background()
	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	admin.SetMaxOpenConns(1)

	pgMu.Lock()
	if pgNoClone {
		pgMu.Unlock()
		return "", false
	}
	if pgTemplate == "" {
		name, err := ensurePGTemplate(ctx, raw, admin)
		if err != nil {
			pgNoClone = true
			pgMu.Unlock()
			t.Logf("storetest: cannot use a template database (%v); migrating per test instead", err)
			return "", false
		}
		pgTemplate = name
	}
	tpl := pgTemplate
	pgMu.Unlock()

	var b [8]byte
	fillRandom(b[:])
	name := "t_" + hex.EncodeToString(b[:])
	// Other test binaries may be cloning the same template right now; CREATE DATABASE
	// refuses to copy a template somebody else is copying, so try again briefly.
	var cerr error
	for range 100 {
		if _, cerr = admin.ExecContext(ctx, `CREATE DATABASE `+name+` TEMPLATE `+tpl); cerr == nil {
			break
		}
		if !strings.Contains(cerr.Error(), "being accessed by other users") {
			t.Fatalf("create database from template: %v", cerr)
		}
		sleepMillis(50)
	}
	if cerr != nil {
		t.Fatalf("create database from template: %v", cerr)
	}
	t.Cleanup(func() {
		a, err := sql.Open("pgx", raw)
		if err != nil {
			return
		}
		defer a.Close()
		_, _ = a.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String(), true
}

// ensurePGTemplate makes (once per migration set, whichever test binary gets
// there first) the migrated template database and returns its name.
func ensurePGTemplate(ctx context.Context, raw string, admin *sql.DB) (string, error) {
	name := "paymux_tpl_" + migrationsHash
	// One advisory lock for the whole machine's test binaries.
	if _, err := admin.ExecContext(ctx, `SELECT pg_advisory_lock(727001)`); err != nil {
		return "", err
	}
	defer admin.ExecContext(ctx, `SELECT pg_advisory_unlock(727001)`)
	var one int
	err := admin.QueryRowContext(ctx, `SELECT 1 FROM pg_database WHERE datname = $1`, name).Scan(&one)
	if err == nil {
		return name, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	// Templates of older migration sets are of no use any more.
	rows, err := admin.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'paymux\_tpl\_%' AND datname <> $1`, name)
	if err == nil {
		var old []string
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				old = append(old, n)
			}
		}
		rows.Close()
		for _, n := range old {
			_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+n+` WITH (FORCE)`)
		}
	}
	building := name + "_building"
	_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+building+` WITH (FORCE)`)
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+building); err != nil {
		return "", err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = "/" + building
	db, err := store.Open(ctx, u.String(), os.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		return "", err
	}
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		db.Close()
		return "", err
	}
	db.Close()
	if _, err := admin.ExecContext(ctx, `ALTER DATABASE `+building+` RENAME TO `+name); err != nil {
		return "", err
	}
	return name, nil
}

func fillRandom(b []byte) { _, _ = rand.Read(b) }

func sleepMillis(n int) { time.Sleep(time.Duration(n) * time.Millisecond) }
