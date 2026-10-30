package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func link(id, slug string) store.PaymentLink {
	return store.PaymentLink{ID: id, OrgID: store.DefaultOrgID, Mode: "test", AppID: "app_a", Slug: slug, Amount: 49900, Currency: "INR",
		Description: "Course fee", Metadata: map[string]string{"sku": "c1"}, Active: true, Version: 1, CreatedAt: t0, UpdatedAt: t0}
}

func TestPaymentLinkRoundTrip(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		fixed := link("plink_01", "slugone")
		fixed.ReturnURL = "https://shop.example.com/thanks"
		fixed.ExpiresAt = t0.Add(48 * time.Hour)
		open := link("plink_02", "slugtwo")
		open.Amount, open.MinAmount, open.MaxAmount, open.SingleUse, open.Metadata = 0, 100, 500000, true, nil
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertPaymentLink(ctx, tx, fixed); err != nil {
				return err
			}
			return store.InsertPaymentLink(ctx, tx, open)
		})
		if err != nil {
			t.Fatal(err)
		}
		var got, gotOpen store.PaymentLink
		err = db.Read(ctx, func(q store.Querier) (err error) {
			if got, err = store.GetPaymentLink(ctx, q, "plink_01"); err != nil {
				return err
			}
			gotOpen, err = store.GetPaymentLinkBySlug(ctx, q, "slugtwo")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Amount != 49900 || got.Currency != "INR" || got.Metadata["sku"] != "c1" || !got.Active || got.SingleUse ||
			!got.ExpiresAt.Equal(fixed.ExpiresAt) || got.ReturnURL != fixed.ReturnURL || got.Slug != "slugone" || got.Version != 1 {
			t.Fatalf("fixed link: %+v", got)
		}
		if gotOpen.Amount != 0 || gotOpen.MinAmount != 100 || gotOpen.MaxAmount != 500000 || !gotOpen.SingleUse || !gotOpen.ExpiresAt.IsZero() || len(gotOpen.Metadata) != 0 {
			t.Fatalf("chooser link: %+v", gotOpen)
		}
		err = db.Read(ctx, func(q store.Querier) error {
			_, err := store.GetPaymentLinkBySlug(ctx, q, "nope")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown slug: %v", err)
		}
	})
}

func TestPaymentLinkSlugIsUniqueAndAmountsAreChecked(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, link("plink_01", "dup")) })
		if err != nil {
			t.Fatal(err)
		}
		err = db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, link("plink_02", "dup")) })
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate slug: %v", err)
		}
		both := link("plink_03", "both") // a fixed amount and a minimum is contradictory
		both.MinAmount = 100
		if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, both) }); err == nil {
			t.Error("the database should refuse a link with a fixed amount and a minimum")
		}
		none := link("plink_04", "none") // neither
		none.Amount = 0
		if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, none) }); err == nil {
			t.Error("the database should refuse a link with no amount and no minimum")
		}
	})
}

func TestListPaymentLinksFiltersAndPages(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		mk := func(id, slug, app, mode string) store.PaymentLink {
			l := link(id, slug)
			l.AppID, l.Mode = app, mode
			return l
		}
		all := []store.PaymentLink{mk("plink_a", "sa", "app_a", "test"), mk("plink_b", "sb", "app_a", "test"), mk("plink_c", "sc", "app_b", "test"),
			mk("plink_d", "sd", "app_a", "live"), mk("plink_e", "se", "app_a", "test")}
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, l := range all {
				if err := store.InsertPaymentLink(ctx, tx, l); err != nil {
					return err
				}
			}
			_, err := store.DeactivatePaymentLink(ctx, tx, "plink_b", "manual", t0.Add(time.Minute))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		ids := func(ls []store.PaymentLink) (out []string) {
			for _, l := range ls {
				out = append(out, l.ID)
			}
			return
		}
		list := func(f store.PaymentLinkFilter) []string {
			var ls []store.PaymentLink
			if err := db.Read(ctx, func(q store.Querier) (err error) { ls, err = store.ListPaymentLinks(ctx, q, f); return }); err != nil {
				t.Fatal(err)
			}
			return ids(ls)
		}
		eq := func(name string, got []string, want ...string) {
			t.Helper()
			if len(got) != len(want) {
				t.Errorf("%s: got %v, want %v", name, got, want)
				return
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("%s: got %v, want %v", name, got, want)
					return
				}
			}
		}
		eq("org, newest first", list(store.PaymentLinkFilter{OrgID: store.DefaultOrgID, Limit: 50}), "plink_e", "plink_d", "plink_c", "plink_b", "plink_a")
		eq("app and mode", list(store.PaymentLinkFilter{OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "test", Limit: 50}), "plink_e", "plink_b", "plink_a")
		active := true
		eq("active only", list(store.PaymentLinkFilter{OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "test", Active: &active, Limit: 50}), "plink_e", "plink_a")
		eq("cursor", list(store.PaymentLinkFilter{OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "test", Limit: 1, StartingAfter: "plink_e"}), "plink_b")
		eq("another org sees nothing", list(store.PaymentLinkFilter{OrgID: "org_other", Limit: 50}))
	})
}

func TestDeactivatePaymentLinkOnlyOnce(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, link("plink_01", "s1")) })
		var first, second bool
		err := db.Write(ctx, func(tx store.Tx) (err error) {
			var l store.PaymentLink
			if l, err = store.DeactivatePaymentLink(ctx, tx, "plink_01", "used", t0.Add(time.Hour)); err != nil {
				return err
			}
			first = !l.Active && l.DeactivatedReason == "used" && l.Version == 2
			l2, err := store.DeactivatePaymentLink(ctx, tx, "plink_01", "manual", t0.Add(2*time.Hour))
			second = errors.Is(err, store.ErrConflict)
			_ = l2
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !first || !second {
			t.Fatalf("first=%v second=%v: the first deactivation wins and a repeat reports ErrConflict", first, second)
		}
		var l store.PaymentLink
		_ = db.Read(ctx, func(q store.Querier) (err error) { l, err = store.GetPaymentLink(ctx, q, "plink_01"); return })
		if l.Active || l.DeactivatedReason != "used" || !l.DeactivatedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("stored: %+v", l)
		}
		err = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.DeactivatePaymentLink(ctx, tx, "plink_missing", "manual", t0)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown link: %v", err)
		}
	})
}

func TestSingleUseClaimIsExclusiveUntilItExpires(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		l := link("plink_01", "s1")
		l.SingleUse = true
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertPaymentLink(ctx, tx, l) })
		claim := func(holder string, now time.Time) bool {
			var ok bool
			if err := db.Write(ctx, func(tx store.Tx) (err error) {
				ok, err = store.ClaimPaymentLink(ctx, tx, "plink_01", holder, now.Add(time.Hour), now)
				return
			}); err != nil {
				t.Fatal(err)
			}
			return ok
		}
		if !claim("holder-a", t0) {
			t.Fatal("the first claim should win")
		}
		if claim("holder-b", t0.Add(10*time.Minute)) {
			t.Error("a second visitor must not claim a link that is in use")
		}
		if !claim("holder-a", t0.Add(10*time.Minute)) {
			t.Error("the holder may claim again (back button, reload)")
		}
		if !claim("holder-b", t0.Add(80*time.Minute)) {
			t.Error("a claim lapses when its time is up")
		}
		_ = db.Write(ctx, func(tx store.Tx) error {
			_, err := store.DeactivatePaymentLink(ctx, tx, "plink_01", "used", t0.Add(90*time.Minute))
			return err
		})
		if claim("holder-b", t0.Add(100*time.Minute)) {
			t.Error("an inactive link cannot be claimed")
		}
	})
}
