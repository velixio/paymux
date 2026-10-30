package store_test

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

// migrationsBefore is the embedded migrations without the ones from `from` on.
func migrationsBefore(t *testing.T, from string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	for _, dir := range []string{"sqlite", "postgres"} {
		entries, err := fs.ReadDir(migrations.FS, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Compare(e.Name(), from) >= 0 {
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

// Migration 0022 gives each existing endpoint a mode: test for an app that never
// went live (no live key, no live payment), live for the rest.
func TestEndpointModeBackfill(t *testing.T) {
	storetest.Each(t, func(t *testing.T, db *store.DB) {
		if err := db.Migrate(ctx, migrationsBefore(t, "0022")); err != nil {
			t.Fatal(err)
		}
		exec := func(q string, args ...any) {
			t.Helper()
			if err := db.Write(ctx, func(tx store.Tx) error { _, err := tx.ExecContext(ctx, q, args...); return err }); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		exec(`INSERT INTO orgs (id, name, created_at) VALUES ('org_1', 'Acme', 1)`)
		for _, a := range []string{"app_never", "app_key", "app_pay"} {
			exec(`INSERT INTO apps (id, org_id, name, slug, created_at, updated_at) VALUES (?, 'org_1', ?, ?, 1, 1)`, a, a, a)
			exec(`INSERT INTO webhook_endpoints (id, org_id, app_id, url, signing_secret_enc, event_types, enabled, created_at, updated_at)
				VALUES (?, 'org_1', ?, 'https://example.com/h', ?, '[]', 1, 1, 1)`, "whe_"+a, a, []byte{1})
		}
		exec(`INSERT INTO api_keys (id, org_id, app_id, mode, prefix, sha256_hash, created_at) VALUES ('key_1', 'org_1', 'app_key', 'live', 'p', 'h', 1)`)
		exec(`INSERT INTO payments (id, org_id, app_id, mode, kind, amount, currency, status, metadata, client_secret_hash, expires_at, version, flags, created_at, updated_at)
			VALUES ('pay_1', 'org_1', 'app_pay', 'live', 'one_time', 100, 'INR', 'created', '{}', 'h', 1, 1, '[]', 1, 1)`)
		if err := db.Migrate(ctx, migrations.FS); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, id := range []string{"whe_app_never", "whe_app_key", "whe_app_pay"} {
			ep, err := db.GetEndpoint(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			got[id] = ep.Mode
		}
		want := map[string]string{"whe_app_never": "test", "whe_app_key": "live", "whe_app_pay": "live"}
		for id, m := range want {
			if got[id] != m {
				t.Errorf("%s mode = %q, want %q", id, got[id], m)
			}
		}
	})
}
