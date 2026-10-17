package store_test

import (
	"errors"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestRenameOrg(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		name := func() (n string) {
			_ = db.Read(ctx, func(q store.Querier) error {
				return q.QueryRowContext(ctx, `SELECT name FROM orgs WHERE id = ?`, store.DefaultOrgID).Scan(&n)
			})
			return
		}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.RenameOrg(ctx, tx, store.DefaultOrgID, "Acme Pay") }); err != nil {
			t.Fatal(err)
		}
		if got := name(); got != "Acme Pay" {
			t.Fatalf("name = %q", got)
		}
		err := db.Write(ctx, func(tx store.Tx) error { return store.RenameOrg(ctx, tx, "org_nope", "X") })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown org: %v", err)
		}
		if got := name(); got != "Acme Pay" {
			t.Fatalf("name changed: %q", got)
		}
	})
}
