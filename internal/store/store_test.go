package store_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

var ctx = context.Background()

func openSQLite(t *testing.T, dir string) (*store.DB, error) {
	t.Helper()
	return store.Open(ctx, "", dir, slog.New(slog.DiscardHandler))
}

func pragma(t *testing.T, db *store.DB, name string) string {
	t.Helper()
	var v string
	err := db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v)
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenSQLiteCreatesDirAndPragmas(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data", "sub")
	db, err := openSQLite(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir stat %v err %v", st, err)
	}
	if got := pragma(t, db, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode=%q", got)
	}
	if got := pragma(t, db, "foreign_keys"); got != "1" {
		t.Errorf("foreign_keys=%q", got)
	}
	if db.Dialect().Name() != "sqlite" {
		t.Error("dialect")
	}
}

func TestSecondSQLiteOpenIsLocked(t *testing.T) {
	dir := t.TempDir()
	first, err := openSQLite(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSQLite(t, dir); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openSQLite(t, dir)
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	second.Close()
}

func TestOpenUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err := openSQLite(t, dir)
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("want error containing %q, got %v", dir, err)
	}
}

func TestOpenPostgresErrorRedactsPassword(t *testing.T) {
	_, err := store.Open(ctx, "postgres://u:s3cret@127.0.0.1:1/db", t.TempDir(), slog.New(slog.DiscardHandler))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "PAYMUX_DATABASE_URL") || !strings.Contains(msg, "127.0.0.1:1") {
		t.Errorf("error should name var and host: %v", msg)
	}
	if strings.Contains(msg, "s3cret") {
		t.Errorf("error leaks password: %v", msg)
	}
}

func TestOpenRejectsUnknownScheme(t *testing.T) {
	_, err := store.Open(ctx, "mysql://x", t.TempDir(), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "PAYMUX_DATABASE_URL") {
		t.Fatalf("got %v", err)
	}
}

func mkTable(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE scratch (id TEXT PRIMARY KEY, n BIGINT)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	err := db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM scratch WHERE n = ?", 1).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func insert(tx store.Tx, id string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO scratch (id, n) VALUES (?, ?)", id, 1)
	return err
}

func TestWriteCommits(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		mkTable(t, db)
		if err := db.Write(ctx, func(tx store.Tx) error { return insert(tx, "a") }); err != nil {
			t.Fatal(err)
		}
		if n := count(t, db); n != 1 {
			t.Fatalf("count=%d", n)
		}
	})
}

func TestWriteRollsBackOnError(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		mkTable(t, db)
		boom := errors.New("boom")
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := insert(tx, "a"); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err=%v", err)
		}
		if n := count(t, db); n != 0 {
			t.Fatalf("count=%d", n)
		}
	})
}

func TestWriteRollsBackOnPanic(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		mkTable(t, db)
		func() {
			defer func() {
				if r := recover(); r != "kaboom" {
					t.Fatalf("recovered %v", r)
				}
			}()
			_ = db.Write(ctx, func(tx store.Tx) error {
				_ = insert(tx, "a")
				panic("kaboom")
			})
		}()
		if n := count(t, db); n != 0 {
			t.Fatalf("count=%d", n)
		}
		// connection must be usable afterwards
		if err := db.Write(ctx, func(tx store.Tx) error { return insert(tx, "b") }); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPing(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		if err := db.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
