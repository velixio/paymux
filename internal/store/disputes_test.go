package store_test

import (
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestCountDisputesNeedingResponse(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			for i, c := range []struct{ org, status string }{
				{store.DefaultOrgID, "needs_response"}, {store.DefaultOrgID, "needs_response"}, {store.DefaultOrgID, "under_review"},
				{store.DefaultOrgID, "won"}, {"org_other", "needs_response"},
			} {
				id := "dsp_" + string(rune('a'+i))
				err := store.InsertDispute(ctx, tx, store.Dispute{ID: id, OrgID: c.org, AppID: "app_a", Mode: "live", PaymentID: "pay_" + id, AttemptID: "att_" + id,
					ProviderAccountID: "pa_x", ProviderDisputeID: "pd_" + id, Amount: 1, Currency: "INR", Status: c.status, Source: "webhook", CreatedAt: t0, UpdatedAt: t0})
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var n, none int
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			if n, err = store.CountDisputesNeedingResponse(ctx, q, store.DefaultOrgID); err != nil {
				return
			}
			none, err = store.CountDisputesNeedingResponse(ctx, q, "org_nobody")
			return
		})
		if n != 2 || none != 0 {
			t.Fatalf("got %d and %d, want 2 and 0", n, none)
		}
	})
}
