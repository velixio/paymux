package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestConnectorKV(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		_ = db.Write(ctx, func(tx store.Tx) error {
			if err := store.ConnectorKVSet(ctx, tx, "pa_1", "k", []byte("v1"), time.Time{}); err != nil {
				t.Fatal(err)
			}
			if err := store.ConnectorKVSet(ctx, tx, "pa_1", "k", []byte("v2"), t0.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			return store.ConnectorKVSet(ctx, tx, "pa_2", "k", []byte("other"), time.Time{})
		})
		get := func(acc string, now time.Time) ([]byte, bool) {
			var v []byte
			var ok bool
			_ = db.Read(ctx, func(q store.Querier) (err error) { v, ok, err = store.ConnectorKVGet(ctx, q, acc, "k", now); return })
			return v, ok
		}
		if v, ok := get("pa_1", t0); !ok || string(v) != "v2" {
			t.Errorf("upsert: %q %v", v, ok)
		}
		if _, ok := get("pa_1", t0.Add(2*time.Hour)); ok {
			t.Error("expired value returned")
		}
		if v, ok := get("pa_2", t0.Add(100*time.Hour)); !ok || string(v) != "other" {
			t.Errorf("isolation/no-expiry: %q %v", v, ok)
		}
		_ = db.Write(ctx, func(tx store.Tx) error { return store.ConnectorKVDelete(ctx, tx, "pa_2", "k") })
		if _, ok := get("pa_2", t0); ok {
			t.Error("delete failed")
		}
	})
}

func TestDomains(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		d := store.Domain{ID: "dom_1", OrgID: "o", AppID: "app_1", Host: "pay.example.org", IsPrimary: true, Status: "pending", Token: "tok", CreatedAt: t0}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertDomain(ctx, tx, d) }); err != nil {
			t.Fatal(err)
		}
		dup := d
		dup.ID = "dom_2"
		if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertDomain(ctx, tx, dup) }); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate host: %v", err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			got, err := store.GetDomainByHost(ctx, q, "pay.example.org")
			if err != nil || got.AppID != "app_1" || !got.IsPrimary || got.Status != "pending" || got.Token != "tok" {
				t.Errorf("get: %+v %v", got, err)
			}
			if _, err := store.GetDomainByHost(ctx, q, "nope.example.org"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("missing: %v", err)
			}
			if _, ok, _ := store.PrimaryActiveHost(ctx, q, "app_1"); ok {
				t.Error("pending domain reported active")
			}
			if n, _ := store.CountDomains(ctx, q, "app_1"); n != 1 {
				t.Errorf("count %d", n)
			}
			return nil
		})
		_ = db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE app_domains SET status = 'active'")
			return err
		})
		_ = db.Read(ctx, func(q store.Querier) error {
			if h, ok, _ := store.PrimaryActiveHost(ctx, q, "app_1"); !ok || h != "pay.example.org" {
				t.Errorf("active host %q %v", h, ok)
			}
			return nil
		})
	})
}

func seedPayment(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertPayment(ctx, tx, store.Payment{ID: "pay_1", OrgID: "o", AppID: "app_1", Mode: "test", Kind: "one_time",
			Amount: 100, Currency: "INR", Status: "created", ClientSecretHash: "x", ExpiresAt: t0, Version: 1, CreatedAt: t0, UpdatedAt: t0}); err != nil {
			return err
		}
		for _, a := range []store.Attempt{
			{ID: "att_1", ProviderRef: "REF1", Status: "created", ProviderOrderID: "order_1"},
			{ID: "att_2", ProviderRef: "REF2", Status: "pending", ProviderPaymentID: "pay_x"},
			{ID: "att_3", ProviderRef: "REF3", Status: "created"}, // never reached a provider
			{ID: "att_4", ProviderRef: "REF4", Status: "captured", ProviderPaymentID: "pay_y"},
		} {
			a.OrgID, a.Mode, a.PaymentID, a.ProviderAccountID, a.CreatedAt, a.UpdatedAt = "o", "test", "pay_1", "pa_1", t0, t0
			if err := store.InsertAttempt(ctx, tx, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFindAttempt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		_ = db.Read(ctx, func(q store.Querier) error {
			for name, c := range map[string]struct{ ref, order, pay, want string }{
				"ref": {"REF2", "", "", "att_2"}, "order": {"", "order_1", "", "att_1"}, "payment": {"", "", "pay_y", "att_4"},
				"first matching key wins": {"REF1", "", "pay_x", "att_1"},
			} {
				a, err := store.FindAttempt(ctx, q, "pa_1", c.ref, c.order, c.pay)
				if err != nil || a.ID != c.want {
					t.Errorf("%s: %+v %v", name, a, err)
				}
			}
			if _, err := store.FindAttempt(ctx, q, "pa_2", "REF1", "order_1", "pay_x"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("other account must not match: %v", err)
			}
			if _, err := store.FindAttempt(ctx, q, "pa_1", "", "", ""); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("empty refs: %v", err)
			}
			return nil
		})
	})
}

func TestDueAttemptsAndBump(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		due := func(now time.Time) map[string]int {
			var items []store.PollItem
			_ = db.Read(ctx, func(q store.Querier) (err error) {
				items, err = store.ListDueAttempts(ctx, q, now, now.Add(-72*time.Hour), 10)
				return
			})
			m := map[string]int{}
			for _, it := range items {
				m[it.ID] = it.PollCount
			}
			return m
		}
		if got := due(t0.Add(30 * time.Second)); len(got) != 0 {
			t.Errorf("nothing is due before the first minute: %v", got)
		}
		got := due(t0.Add(time.Minute))
		if len(got) != 2 || got["att_1"] != 0 || got["att_2"] != 0 {
			t.Errorf("due after 1m (open attempts with a provider reference only): %v", got)
		}
		_ = db.Write(ctx, func(tx store.Tx) error {
			return store.BumpAttemptPoll(ctx, tx, "att_1", 1, t0.Add(3*time.Minute))
		})
		got = due(t0.Add(2 * time.Minute))
		if _, ok := got["att_1"]; ok || len(got) != 1 {
			t.Errorf("bumped attempt still due: %v", got)
		}
		if got = due(t0.Add(3 * time.Minute)); got["att_1"] != 1 {
			t.Errorf("poll count lost: %v", got)
		}
		if got = due(t0.Add(73 * time.Hour)); len(got) != 0 {
			t.Errorf("attempts older than 72h must stop being polled: %v", got)
		}
	})
}

func TestFindRefundScopedToAccount(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		err := db.Write(ctx, func(tx store.Tx) error {
			for i, acc := range []string{"pa_1", "pa_2"} {
				n := string(rune('1' + i))
				if err := store.InsertPayment(ctx, tx, store.Payment{ID: "pay_" + n, OrgID: "o", AppID: "app_1", Mode: "test", Kind: "one_time",
					Amount: 100, Currency: "INR", Status: "created", ClientSecretHash: "x", ExpiresAt: t0, Version: 1, CreatedAt: t0, UpdatedAt: t0}); err != nil {
					return err
				}
				if err := store.InsertAttempt(ctx, tx, store.Attempt{ID: "att_" + n, OrgID: "o", Mode: "test", PaymentID: "pay_" + n,
					ProviderAccountID: acc, ProviderRef: "AREF" + n, Status: "captured", CreatedAt: t0, UpdatedAt: t0}); err != nil {
					return err
				}
				if err := store.InsertRefund(ctx, tx, store.Refund{ID: "ref_" + n, OrgID: "o", AppID: "app_1", Mode: "test", PaymentID: "pay_" + n,
					AttemptID: "att_" + n, Amount: 10, Currency: "INR", Status: "pending", ProviderRef: "RREF" + n, ProviderRefundID: "rfnd_same",
					InitiatedBy: "api", CreatedAt: t0, UpdatedAt: t0}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			for acc, want := range map[string]string{"pa_1": "ref_1", "pa_2": "ref_2"} {
				got, err := store.FindRefund(ctx, q, acc, "", "rfnd_same")
				if err != nil || got.ID != want {
					t.Errorf("%s by provider refund id: %+v %v", acc, got, err)
				}
			}
			if _, err := store.FindRefund(ctx, q, "pa_1", "RREF2", ""); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("another account's provider_ref matched: %v", err)
			}
			if _, err := store.FindRefund(ctx, q, "pa_3", "RREF1", "rfnd_same"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("third account matched: %v", err)
			}
			return nil
		})
	})
}

func TestRefundHelpers(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedPayment(t, db)
		r := store.Refund{ID: "ref_1", OrgID: "o", AppID: "app_1", Mode: "test", PaymentID: "pay_1", AttemptID: "att_1", Amount: 10,
			Currency: "INR", Status: "pending", ProviderRef: "RREF", InitiatedBy: "api", CreatedAt: t0, UpdatedAt: t0}
		_ = db.Write(ctx, func(tx store.Tx) error { return store.InsertRefund(ctx, tx, r) })
		_ = db.Write(ctx, func(tx store.Tx) error {
			r.Status, r.ProviderRefundID = "succeeded", "rfnd_1"
			return store.UpdateRefund(ctx, tx, r, t0)
		})
		_ = db.Read(ctx, func(q store.Querier) error {
			for _, c := range []struct{ ref, id string }{{"RREF", ""}, {"", "rfnd_1"}} {
				got, err := store.FindRefund(ctx, q, "pa_1", c.ref, c.id)
				if err != nil || got.ID != "ref_1" || got.Status != "succeeded" || got.ProviderRefundID != "rfnd_1" {
					t.Errorf("FindRefund(%v): %+v %v", c, got, err)
				}
			}
			if _, err := store.FindRefund(ctx, q, "pa_1", "nope", "nope"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("missing: %v", err)
			}
			return nil
		})
	})
}

func TestProviderAccountRoundTripAndUnknownObjects(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		pa := store.ProviderAccount{ID: "pa_1", OrgID: "o", Mode: "test", Connector: "razorpay", Name: "main", Status: "active",
			ConfigEnc: []byte{1, 2, 3}, AcceptedCurrencies: []string{"INR", "USD"}, ProviderGSTIN: "27AAAAA0000A1Z5", CreatedAt: t0}
		_ = db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertProviderAccount(ctx, tx, pa); err != nil {
				return err
			}
			if err := store.LinkAppProviderAccount(ctx, tx, "app_1", "pa_1"); err != nil {
				return err
			}
			return store.LinkAppProviderAccount(ctx, tx, "app_1", "pa_1") // idempotent
		})
		_ = db.Read(ctx, func(q store.Querier) error {
			got, err := store.GetProviderAccount(ctx, q, "pa_1")
			if err != nil || got.Connector != "razorpay" || len(got.AcceptedCurrencies) != 2 || got.EnabledPurposes[0] != "one_time" ||
				string(got.ConfigEnc) != "\x01\x02\x03" || got.ProviderGSTIN != "27AAAAA0000A1Z5" {
				t.Errorf("get: %+v %v", got, err)
			}
			if l, _ := store.ListAppProviderAccounts(ctx, q, "app_1", "test"); len(l) != 1 {
				t.Errorf("list test: %v", l)
			}
			if l, _ := store.ListAppProviderAccounts(ctx, q, "app_1", "live"); len(l) != 0 {
				t.Errorf("list live: %v", l)
			}
			if l, _ := store.ListAppProviderAccounts(ctx, q, "app_2", "test"); len(l) != 0 {
				t.Errorf("list other app: %v", l)
			}
			return nil
		})
		pa.Name, pa.Status, pa.ConfigEnc, pa.AcceptedCurrencies, pa.EnabledPurposes = "renamed", "disabled", []byte{9}, []string{"INR"}, []string{"one_time", "subscription"}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.UpdateProviderAccount(ctx, tx, pa, t0) }); err != nil {
			t.Fatal(err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			got, _ := store.GetProviderAccount(ctx, q, "pa_1")
			if got.Name != "renamed" || got.Status != "disabled" || len(got.EnabledPurposes) != 2 || got.ConfigEnc[0] != 9 {
				t.Errorf("update: %+v", got)
			}
			return nil
		})
		pa.ID = "pa_missing"
		if err := db.Write(ctx, func(tx store.Tx) error { return store.UpdateProviderAccount(ctx, tx, pa, t0) }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("update missing: %v", err)
		}
		u := store.UnknownObject{ID: "uo_1", ProviderAccountID: "pa_1", ProviderEventID: "evt", Kind: "payment", ProviderID: "pay_zzz", Detail: "{}", CreatedAt: t0}
		_ = db.Write(ctx, func(tx store.Tx) error {
			_ = store.InsertUnknownObject(ctx, tx, u)
			u.ID = "uo_2"
			return store.InsertUnknownObject(ctx, tx, u) // same key: ignored
		})
		_ = db.Read(ctx, func(q store.Querier) error {
			l, err := store.ListUnknownObjects(ctx, q, "pa_1")
			if err != nil || len(l) != 1 || l[0].ProviderID != "pay_zzz" {
				t.Errorf("unknown objects: %+v %v", l, err)
			}
			return nil
		})
	})
}

func TestProviderEventHelpers(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		row := store.ProviderEventRow{ID: "pev_1", ProviderAccountID: "pa_1", ProviderEventID: "e1", Headers: []byte(`{}`), Body: []byte("b"), Verified: true, ReceivedAt: t0}
		_ = db.Write(ctx, func(tx store.Tx) error { _, err := db.InsertProviderEvent(ctx, tx, row); return err })
		_ = db.Write(ctx, func(tx store.Tx) error {
			return store.MarkProviderEventProcessed(ctx, tx, "pev_1", t0.Add(time.Second), "boom")
		})
		_ = db.Read(ctx, func(q store.Querier) error {
			got, err := store.GetProviderEventByID(ctx, q, "pev_1")
			if err != nil || got.ProcessedAt.IsZero() || got.Error != "boom" || string(got.Body) != "b" || !got.Verified {
				t.Errorf("%+v %v", got, err)
			}
			if _, err := store.GetProviderEventByID(ctx, q, "nope"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("missing: %v", err)
			}
			return nil
		})
	})
}
