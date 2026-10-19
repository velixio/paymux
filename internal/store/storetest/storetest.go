// Package storetest runs a test body against every supported database.
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/store"
)

// SQLite opens a fresh SQLite store in a temp dir.
func SQLite(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), "", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// PostgresURL creates a fresh random schema and returns a database URL that
// uses it; the schema is dropped on cleanup. Without PAYMUX_TEST_POSTGRES_URL
// it skips, or fails if PAYMUX_REQUIRE_PG=1.
func PostgresURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("PAYMUX_TEST_POSTGRES_URL")
	if raw == "" {
		if os.Getenv("PAYMUX_REQUIRE_PG") == "1" {
			t.Fatal("PAYMUX_REQUIRE_PG=1 but PAYMUX_TEST_POSTGRES_URL is unset")
		}
		t.Skip("PAYMUX_TEST_POSTGRES_URL is unset")
	}
	ctx := context.Background()
	var b [8]byte
	_, _ = rand.Read(b[:])
	schema := "t_" + hex.EncodeToString(b[:])

	admin, err := sql.Open("pgx", raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") })

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// Postgres opens a store on a fresh random schema.
func Postgres(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), PostgresURL(t), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() }) // runs before schema drop (LIFO)
	return db
}

// Each runs fn as subtests "sqlite" and "postgres".
func Each(t *testing.T, fn func(t *testing.T, db *store.DB)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, SQLite(t)) })
	t.Run("postgres", func(t *testing.T) { fn(t, Postgres(t)) })
}

// EachMigrated is Each with every migration already applied.
func EachMigrated(t *testing.T, fn func(t *testing.T, db *store.DB)) {
	t.Helper()
	Each(t, func(t *testing.T, db *store.DB) {
		if err := db.Migrate(context.Background(), migrations.FS); err != nil {
			t.Fatal(err)
		}
		fn(t, db)
	})
}
