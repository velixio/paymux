package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func TestSeedOrgIsIdempotent(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		for i := 0; i < 3; i++ {
			if err := db.Write(ctx, func(tx store.Tx) error { return store.SeedOrg(ctx, tx, "Paymux", t0) }); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		err := db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COUNT(*) FROM orgs").Scan(&n)
		})
		if err != nil || n != 1 {
			t.Fatalf("orgs=%d err=%v", n, err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			id, err := store.FirstOrgID(ctx, q)
			if err != nil || id != store.DefaultOrgID {
				t.Errorf("FirstOrgID=%q %v", id, err)
			}
			return nil
		})
	})
}

func TestAppAndKeyRoundTrip(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: "o", Name: "A", Slug: "a", CreatedAt: t0}); err != nil {
				return err
			}
			return store.InsertAPIKey(ctx, tx, store.APIKey{ID: "key_1", OrgID: "o", AppID: "app_1", Mode: "test", Prefix: "pmx_test_sk_abcd", Hash: "h", CreatedAt: t0})
		})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Write(ctx, func(tx store.Tx) error {
			return store.InsertApp(ctx, tx, store.App{ID: "app_2", OrgID: "o", Name: "B", Slug: "a", CreatedAt: t0})
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate slug: %v", err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			a, err := store.GetAppBySlug(ctx, q, "o", "a")
			if err != nil || a.ID != "app_1" {
				t.Errorf("app %+v %v", a, err)
			}
			if _, err := store.GetApp(ctx, q, "nope"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("missing app: %v", err)
			}
			k, err := store.GetAPIKeyByPrefix(ctx, q, "pmx_test_sk_abcd")
			if err != nil || k.AppID != "app_1" || !k.RevokedAt.IsZero() || !k.LastUsedAt.IsZero() {
				t.Errorf("key %+v %v", k, err)
			}
			return nil
		})
	})
}

func TestPaymentVersionedUpdateAndPaging(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		mk := func(id string) store.Payment {
			return store.Payment{ID: id, OrgID: "o", AppID: "app_1", Mode: "test", Kind: "one_time", Amount: 100, Currency: "INR",
				Status: "created", Metadata: map[string]string{"k": "v"}, ClientSecretHash: "x", ExpiresAt: t0, Version: 1, CreatedAt: t0, UpdatedAt: t0}
		}
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, id := range []string{"pay_1", "pay_2", "pay_3"} {
				if err := store.InsertPayment(ctx, tx, mk(id)); err != nil {
					return err
				}
			}
			p, err := store.GetPaymentForUpdate(ctx, tx, "pay_2")
			if err != nil {
				return err
			}
			p.Status = "succeeded"
			p.Flags = []string{"f"}
			if err := store.UpdatePayment(ctx, tx, &p, t0); err != nil || p.Version != 2 {
				t.Errorf("update: v=%d %v", p.Version, err)
			}
			stale := p
			stale.Version = 1
			if err := store.UpdatePayment(ctx, tx, &stale, t0); !errors.Is(err, store.ErrConflict) {
				t.Errorf("stale update: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			page, err := store.ListPayments(ctx, q, "app_1", "test", "", "", 2)
			if err != nil || len(page) != 2 || page[0].ID != "pay_3" || page[1].ID != "pay_2" {
				t.Fatalf("page1 %v %v", page, err)
			}
			page, _ = store.ListPayments(ctx, q, "app_1", "test", "", "pay_2", 2)
			if len(page) != 1 || page[0].ID != "pay_1" {
				t.Errorf("page2 %v", page)
			}
			page, _ = store.ListPayments(ctx, q, "app_1", "test", "succeeded", "", 10)
			if len(page) != 1 || !page[0].HasFlag("f") || page[0].Metadata["k"] != "v" {
				t.Errorf("filtered %+v", page)
			}
			if page, _ = store.ListPayments(ctx, q, "app_1", "live", "", "", 10); len(page) != 0 {
				t.Errorf("mode leak %v", page)
			}
			return nil
		})
	})
}

func TestIdempotencyReserveFlow(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		reserve := func(hash string, now time.Time) *store.IdempotencyRecord {
			var rec *store.IdempotencyRecord
			err := db.Write(ctx, func(tx store.Tx) (err error) {
				rec, err = store.ReserveIdempotencyKey(ctx, tx, "app_1", "test", "k1", hash, now, 24*time.Hour, time.Minute)
				return
			})
			if err != nil {
				t.Fatal(err)
			}
			return rec
		}
		if rec := reserve("h1", t0); rec != nil {
			t.Fatalf("first reserve should own the key: %+v", rec)
		}
		if rec := reserve("h1", t0.Add(time.Second)); rec == nil || rec.Status != 0 {
			t.Fatalf("in-progress record expected: %+v", rec)
		}
		_ = db.Write(ctx, func(tx store.Tx) error {
			return store.CompleteIdempotencyKey(ctx, tx, "app_1", "test", "k1", 201, []byte("resp"))
		})
		if rec := reserve("h1", t0.Add(time.Hour)); rec == nil || rec.Status != 201 || string(rec.ResponseEnc) != "resp" || rec.RequestHash != "h1" {
			t.Fatalf("stored record expected: %+v", rec)
		}
		if rec := reserve("h2", t0.Add(25*time.Hour)); rec != nil {
			t.Fatalf("expired key should be reusable: %+v", rec)
		}
		// A stale in-progress reservation (crashed request) is taken over.
		if rec := reserve("h2", t0.Add(25*time.Hour+10*time.Second)); rec == nil {
			t.Fatal("fresh in-progress key must block")
		}
		if rec := reserve("h2", t0.Add(25*time.Hour+2*time.Minute)); rec != nil {
			t.Fatalf("stale in-progress key should be taken over: %+v", rec)
		}
	})
}

func TestSettingsAndAuditStore(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.SeedOrgSetting(ctx, tx, "o", "k", `"a"`, t0); err != nil {
				return err
			}
			if err := store.SeedOrgSetting(ctx, tx, "o", "k", `"b"`, t0); err != nil { // no overwrite
				return err
			}
			if err := store.SetAppSetting(ctx, tx, "app_1", "k", `"c"`, t0); err != nil {
				return err
			}
			if err := store.SetAppSetting(ctx, tx, "app_1", "k", `"d"`, t0); err != nil {
				return err
			}
			return store.InsertAudit(ctx, tx, store.AuditRow{ID: "aud_1", OrgID: "o", ActorType: "system", Action: "x", At: t0})
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			if v, ok, _ := store.GetOrgSetting(ctx, q, "o", "k"); !ok || v != `"a"` {
				t.Errorf("org %q %v", v, ok)
			}
			if v, ok, _ := store.GetAppSetting(ctx, q, "app_1", "k"); !ok || v != `"d"` {
				t.Errorf("app %q %v", v, ok)
			}
			if _, ok, _ := store.GetAppSetting(ctx, q, "app_2", "k"); ok {
				t.Error("unexpected app value")
			}
			rows, err := store.ListAudit(ctx, q, "o", "", 10)
			if err != nil || len(rows) != 1 || rows[0].Action != "x" {
				t.Errorf("audit %+v %v", rows, err)
			}
			return nil
		})
		_ = db.Write(ctx, func(tx store.Tx) error { return store.DeleteAppSetting(ctx, tx, "app_1", "k") })
		_ = db.Read(ctx, func(q store.Querier) error {
			if _, ok, _ := store.GetAppSetting(ctx, q, "app_1", "k"); ok {
				t.Error("override not deleted")
			}
			return nil
		})
	})
}
