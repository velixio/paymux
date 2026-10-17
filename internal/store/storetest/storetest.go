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

	"github.com/paymux/paymux/internal/store"
)

// Each runs fn as subtests "sqlite" and "postgres".
func Each(t *testing.T, fn func(t *testing.T, db *store.DB)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		db, err := store.Open(context.Background(), "", t.TempDir(), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		fn(t, db)
	})
	t.Run("postgres", func(t *testing.T) {
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

		db, err := store.Open(ctx, u.String(), t.TempDir(), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() }) // runs before schema drop (LIFO)
		fn(t, db)
	})
}
