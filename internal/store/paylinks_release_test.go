package store_test

import (
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestReleasePaymentLinkFreesOnlyTheHoldersClaim(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		l := link("plink_01", "s1")
		l.SingleUse = true
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, l) })
		claim := func(holder string) (ok bool) {
			_ = db.Write(ctx, func(tx store.Tx) (err error) {
				ok, err = store.ClaimPaymentLink(ctx, tx, "plink_01", holder, t0.Add(time.Hour), t0)
				return
			})
			return
		}
		release := func(holder string) {
			if err := db.Write(ctx, func(tx store.Tx) error { return store.ReleasePaymentLink(ctx, tx, "plink_01", holder, t0) }); err != nil {
				t.Fatal(err)
			}
		}
		if !claim("a") {
			t.Fatal("claim")
		}
		release("b") // not the holder: nothing changes
		if claim("c") {
			t.Fatal("a stranger's release must not free the link")
		}
		release("a")
		if !claim("c") {
			t.Fatal("the holder's release frees the link")
		}
	})
}
