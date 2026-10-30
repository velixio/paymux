package store_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func seedBase(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", t0); err != nil {
			return err
		}
		for _, a := range []store.App{
			{ID: "app_a", OrgID: store.DefaultOrgID, Name: "Alpha", Slug: "alpha", CreatedAt: t0},
			{ID: "app_b", OrgID: store.DefaultOrgID, Name: "Beta", Slug: "beta", CreatedAt: t0.Add(time.Second)},
		} {
			if err := store.InsertApp(ctx, tx, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAppDashboardStore(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		var apps []store.App
		_ = db.Read(ctx, func(q store.Querier) (err error) { apps, err = store.ListApps(ctx, q, store.DefaultOrgID); return })
		if len(apps) != 2 || apps[0].ID != "app_a" || apps[1].ID != "app_b" {
			t.Fatalf("apps %+v", apps)
		}

		// branding and return URL
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.SetAppBranding(ctx, tx, "app_a", `{"name":"Alpha Pay","accent":"#112233"}`, t0); err != nil {
				return err
			}
			return store.SetAppReturnURL(ctx, tx, "app_a", "https://alpha.example.com/back", t0)
		})
		if err != nil {
			t.Fatal(err)
		}
		var br string
		var a store.App
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			br, err = store.GetAppBranding(ctx, q, "app_a")
			if err != nil {
				return
			}
			a, err = store.GetApp(ctx, q, "app_a")
			return
		})
		if br != `{"name":"Alpha Pay","accent":"#112233"}` || a.DefaultReturnURL != "https://alpha.example.com/back" {
			t.Fatalf("branding %q app %+v", br, a)
		}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.SetAppBranding(ctx, tx, "nope", `{}`, t0) }); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("branding of missing app: %v", err)
		}

		// api keys: list and revoke
		err = db.Write(ctx, func(tx store.Tx) error {
			for i, id := range []string{"key_1", "key_2"} {
				k := store.APIKey{ID: id, OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "test", Prefix: "pmx_test_sk_abcdefg" + string(rune('0'+i)), Hash: "h", CreatedAt: t0.Add(time.Duration(i) * time.Second)}
				if err := store.InsertAPIKey(ctx, tx, k); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var keys []store.APIKey
		_ = db.Read(ctx, func(q store.Querier) (err error) { keys, err = store.ListAPIKeys(ctx, q, "app_a"); return })
		if len(keys) != 2 || keys[0].ID != "key_1" || !keys[0].RevokedAt.IsZero() {
			t.Fatalf("keys %+v", keys)
		}
		var ok bool
		_ = db.Write(ctx, func(tx store.Tx) (err error) {
			ok, err = store.RevokeAPIKey(ctx, tx, "app_a", "key_1", t0.Add(time.Hour))
			return
		})
		if !ok {
			t.Fatal("revoke reported nothing")
		}
		_ = db.Write(ctx, func(tx store.Tx) (err error) {
			ok, err = store.RevokeAPIKey(ctx, tx, "app_a", "key_1", t0.Add(2*time.Hour))
			return
		})
		if ok {
			t.Fatal("second revoke must be a no-op")
		}
		_ = db.Write(ctx, func(tx store.Tx) (err error) { ok, err = store.RevokeAPIKey(ctx, tx, "app_b", "key_2", t0); return })
		if ok {
			t.Fatal("revoked another app's key")
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) { keys, err = store.ListAPIKeys(ctx, q, "app_a"); return })
		if !keys[0].RevokedAt.Equal(t0.Add(time.Hour)) || !keys[1].RevokedAt.IsZero() {
			t.Fatalf("revoked_at %+v", keys)
		}
	})
}

func TestBlobStore(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		b := store.Blob{ID: "blob_1", OrgID: store.DefaultOrgID, Kind: "logo", ContentType: "image/png", Data: []byte{0, 1, 2, 255}, CreatedAt: t0}
		if err := db.Write(ctx, func(tx store.Tx) error { return store.InsertBlob(ctx, tx, b) }); err != nil {
			t.Fatal(err)
		}
		var got store.Blob
		_ = db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetBlob(ctx, q, "blob_1"); return })
		if got.ContentType != "image/png" || got.Size != 4 || !reflect.DeepEqual(got.Data, b.Data) || got.Kind != "logo" {
			t.Fatalf("%+v", got)
		}
		err := db.Read(ctx, func(q store.Querier) error { _, err := store.GetBlob(ctx, q, "x"); return err })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

func TestProviderAccountLists(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, pa := range []store.ProviderAccountInfo{
				{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "test", Connector: "razorpay", Name: "RZP test", Status: "active", Currencies: []string{"INR"}, Purposes: []string{"one_time"}},
				{ID: "pa_2", OrgID: store.DefaultOrgID, Mode: "live", Connector: "whop", Name: "Whop", Status: "active", Currencies: []string{"USD", "BDT"}, Purposes: []string{"one_time", "subscription"}},
			} {
				if err := store.InsertProviderAccountInfo(ctx, tx, pa, t0); err != nil {
					return err
				}
			}
			return store.SetAppProviderAccounts(ctx, tx, "app_a", []string{"pa_1", "pa_2"})
		})
		if err != nil {
			t.Fatal(err)
		}
		var all, live []store.ProviderAccountInfo
		var allowed []string
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			if all, err = store.ListProviderAccounts(ctx, q, store.DefaultOrgID, ""); err != nil {
				return
			}
			if live, err = store.ListProviderAccounts(ctx, q, store.DefaultOrgID, "live"); err != nil {
				return
			}
			allowed, err = store.ListAppProviderAccountIDs(ctx, q, "app_a")
			return
		})
		if len(all) != 2 || len(live) != 1 || live[0].ID != "pa_2" || !reflect.DeepEqual(live[0].Currencies, []string{"USD", "BDT"}) ||
			!reflect.DeepEqual(live[0].Purposes, []string{"one_time", "subscription"}) {
			t.Fatalf("lists %+v %+v", all, live)
		}
		if !reflect.DeepEqual(allowed, []string{"pa_1", "pa_2"}) {
			t.Fatalf("allowed %v", allowed)
		}
		_ = db.Write(ctx, func(tx store.Tx) error { return store.SetAppProviderAccounts(ctx, tx, "app_a", []string{"pa_2"}) })
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			allowed, err = store.ListAppProviderAccountIDs(ctx, q, "app_a")
			return
		})
		if !reflect.DeepEqual(allowed, []string{"pa_2"}) {
			t.Fatalf("replace: %v", allowed)
		}
	})
}

func pay(id, app, mode, status, orderRef, cust string, created time.Time) store.Payment {
	return store.Payment{ID: id, OrgID: store.DefaultOrgID, AppID: app, Mode: mode, Kind: "one_time", Amount: 1000, Currency: "INR",
		Status: status, OrderRef: orderRef, CustomerID: cust, Metadata: map[string]string{}, ClientSecretHash: "h", ExpiresAt: created.Add(time.Hour),
		Version: 1, Flags: []string{}, CreatedAt: created, UpdatedAt: created}
}

// orgPay moves a payment into another org.
func orgPay(p store.Payment) store.Payment { p.OrgID = "org_other"; return p }

func TestSearchPayments(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			if _, err := store.UpsertCustomer(ctx, tx, store.Customer{ID: "cus_1", OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "test", Email: "Pri.Ya@Example.com"}, t0); err != nil {
				return err
			}
			for _, p := range []store.Payment{
				pay("pay_01", "app_a", "test", "succeeded", "ORD-100", "cus_1", t0),
				pay("pay_02", "app_a", "test", "failed", "ORD-200", "", t0.Add(24*time.Hour)),
				pay("pay_03", "app_b", "test", "succeeded", "ORD_300", "", t0.Add(48*time.Hour)),
				pay("pay_04", "app_a", "live", "succeeded", "ORD-400", "", t0.Add(72*time.Hour)),
				orgPay(pay("pay_05", "app_x", "test", "succeeded", "ORD-500", "", t0.Add(96*time.Hour))),
			} {
				if err := store.InsertPayment(ctx, tx, p); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		search := func(f store.PaymentFilter, after string, limit int) []string {
			var ids []string
			_ = db.Read(ctx, func(q store.Querier) error {
				rows, err := store.SearchPayments(ctx, q, f, after, limit)
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range rows {
					ids = append(ids, p.ID)
				}
				return nil
			})
			return ids
		}
		cases := []struct {
			name string
			f    store.PaymentFilter
			want []string
		}{
			{"mode only", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test"}, []string{"pay_03", "pay_02", "pay_01"}},
			{"app", store.PaymentFilter{OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "test"}, []string{"pay_02", "pay_01"}},
			{"status", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", Status: "succeeded"}, []string{"pay_03", "pay_01"}},
			{"from", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", From: t0.Add(12 * time.Hour)}, []string{"pay_03", "pay_02"}},
			{"to exclusive", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", To: t0.Add(24 * time.Hour)}, []string{"pay_01"}},
			{"search id prefix", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", Search: "pay_02"}, []string{"pay_02"}},
			{"search order ref", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", Search: "ord-100"}, []string{"pay_01"}},
			{"search email", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", Search: "pri.ya@example"}, []string{"pay_01"}},
			{"like wildcard is literal", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", Search: "ORD_300"}, []string{"pay_03"}},
			{"percent is literal", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", Search: "%"}, nil},
			{"underscore is literal", store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test", Search: "ORD_"}, []string{"pay_03"}},
		}
		for _, c := range cases {
			if got := search(c.f, "", 10); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: got %v want %v", c.name, got, c.want)
			}
		}
		if got := search(store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test"}, "pay_03", 10); !reflect.DeepEqual(got, []string{"pay_02", "pay_01"}) {
			t.Errorf("cursor: %v", got)
		}
		if got := search(store.PaymentFilter{OrgID: store.DefaultOrgID, Mode: "test"}, "", 2); len(got) != 2 {
			t.Errorf("limit: %v", got)
		}
	})
}

func TestLedgerTotals(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, p := range []store.Payment{
				pay("pay_1", "app_a", "test", "succeeded", "", "", t0), pay("pay_2", "app_b", "test", "succeeded", "", "", t0),
				pay("pay_3", "app_a", "live", "succeeded", "", "", t0),
			} {
				if err := store.InsertPayment(ctx, tx, p); err != nil {
					return err
				}
			}
			for i, e := range []store.LedgerEntry{
				{Kind: "charge", Amount: 10000, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0},
				{Kind: "charge", Amount: 5000, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(time.Hour)},
				{Kind: "refund", Amount: -3000, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(2 * time.Hour)},
				{Kind: "charge", Amount: 700, Currency: "USD", PaymentID: "pay_2", Mode: "test", OccurredAt: t0},
				{Kind: "charge", Amount: 99, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(-48 * time.Hour)}, // before the window
				{Kind: "fee", Amount: -50, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0},                        // not gross/refund
				{Kind: "charge", Amount: 1234, Currency: "INR", PaymentID: "pay_3", Mode: "live", OccurredAt: t0},                    // other mode
			} {
				e.ID, e.OrgID = "led_"+string(rune('a'+i)), store.DefaultOrgID
				if err := store.InsertLedgerEntry(ctx, tx, e); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []store.LedgerTotal
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			got, err = store.LedgerTotals(ctx, q, store.DefaultOrgID, "test", t0.Add(-time.Hour))
			return
		})
		want := []store.LedgerTotal{
			{AppID: "app_a", Currency: "INR", Gross: 15000, Refunds: 3000},
			{AppID: "app_b", Currency: "USD", Gross: 700},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})
}

func TestSearchAudit(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			for i, r := range []store.AuditRow{
				{ID: "aud_1", ActorType: "user", ActorID: "usr_a", Action: "refund.create", Target: "ref_1", At: t0},
				{ID: "aud_2", ActorType: "user", ActorID: "usr_b", Action: "setting.update", Target: "org:x/payment_ttl", At: t0.Add(time.Hour)},
				{ID: "aud_3", ActorType: "api_key", ActorID: "key_1", Action: "refund.create", Target: "ref_2", At: t0.Add(2 * time.Hour)},
				{ID: "aud_4", ActorType: "system", Action: "app.create", At: t0.Add(3 * time.Hour)},
			} {
				r.OrgID = store.DefaultOrgID
				_ = i
				if err := store.InsertAudit(ctx, tx, r); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ids := func(f store.AuditFilter, limit int) []string {
			var out []string
			_ = db.Read(ctx, func(q store.Querier) error {
				rows, err := store.SearchAudit(ctx, q, f, limit)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					out = append(out, r.ID)
				}
				return nil
			})
			return out
		}
		org := store.DefaultOrgID
		for name, c := range map[string]struct {
			f    store.AuditFilter
			want []string
		}{
			"all newest first": {store.AuditFilter{OrgID: org}, []string{"aud_4", "aud_3", "aud_2", "aud_1"}},
			"actor":            {store.AuditFilter{OrgID: org, Actor: "usr_a"}, []string{"aud_1"}},
			"actor type":       {store.AuditFilter{OrgID: org, Actor: "system"}, []string{"aud_4"}},
			"action":           {store.AuditFilter{OrgID: org, Action: "refund.create"}, []string{"aud_3", "aud_1"}},
			"action prefix":    {store.AuditFilter{OrgID: org, Action: "setting."}, []string{"aud_2"}},
			"from":             {store.AuditFilter{OrgID: org, From: t0.Add(90 * time.Minute)}, []string{"aud_4", "aud_3"}},
			"to":               {store.AuditFilter{OrgID: org, To: t0.Add(time.Hour)}, []string{"aud_1"}},
			"after cursor":     {store.AuditFilter{OrgID: org, After: "aud_3"}, []string{"aud_2", "aud_1"}},
			"other org":        {store.AuditFilter{OrgID: "org_x"}, nil},
			"target":           {store.AuditFilter{OrgID: org, Target: "ref_2"}, []string{"aud_3"}},
		} {
			if got := ids(c.f, 10); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: got %v want %v", name, got, c.want)
			}
		}
		if got := ids(store.AuditFilter{OrgID: org}, 2); len(got) != 2 {
			t.Errorf("limit: %v", got)
		}
	})
}

func TestPaymentEventsAndDeliveries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			if err := store.InsertPayment(ctx, tx, pay("pay_1", "app_a", "test", "succeeded", "", "", t0)); err != nil {
				return err
			}
			if err := store.InsertRefund(ctx, tx, store.Refund{ID: "ref_1", OrgID: store.DefaultOrgID, AppID: "app_a", Mode: "test", PaymentID: "pay_1",
				AttemptID: "att_1", Amount: 10, Currency: "INR", Status: "pending", ProviderRef: "pr_1", InitiatedBy: "api", CreatedAt: t0, UpdatedAt: t0}); err != nil {
				return err
			}
			for _, e := range []store.EventRow{
				{ID: "evt_1", Type: "payment.succeeded", ObjectType: "payment", ObjectID: "pay_1", CreatedAt: t0},
				{ID: "evt_2", Type: "refund.created", ObjectType: "refund", ObjectID: "ref_1", CreatedAt: t0.Add(time.Second)},
				{ID: "evt_3", Type: "payment.succeeded", ObjectType: "payment", ObjectID: "pay_other", CreatedAt: t0.Add(2 * time.Second)},
			} {
				e.OrgID, e.AppID, e.Mode, e.ObjectVersion, e.Data = store.DefaultOrgID, "app_a", "test", 1, json.RawMessage(`{}`)
				if err := db.InsertEvent(ctx, tx, e); err != nil {
					return err
				}
			}
			if err := db.InsertEndpoint(ctx, tx, store.EndpointRow{ID: "whe_1", OrgID: store.DefaultOrgID, AppID: "app_a", URL: "https://example.com/h",
				SigningSecretEnc: []byte{1}, Enabled: true, CreatedAt: t0, UpdatedAt: t0}); err != nil {
				return err
			}
			for i, d := range []store.DeliveryRow{
				{ID: "whd_1", EventID: "evt_1", Status: "delivered", AttemptCount: 1, LastStatus: 200, DeliveredAt: t0},
				{ID: "whd_2", EventID: "evt_2", Status: "failed", AttemptCount: 9, LastStatus: 500, LastError: "http 500"},
				{ID: "whd_3", EventID: "evt_3", Status: "pending"},
			} {
				d.EndpointID, d.CreatedAt, d.UpdatedAt = "whe_1", t0.Add(time.Duration(i)*time.Second), t0.Add(time.Duration(i)*time.Second)
				if err := db.InsertDelivery(ctx, tx, d); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var evs []store.EventRow
		var dels, epDels []store.DeliveryListRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			if evs, err = store.ListPaymentEvents(ctx, q, "pay_1"); err != nil {
				return
			}
			if dels, err = store.ListPaymentDeliveries(ctx, q, "pay_1"); err != nil {
				return
			}
			epDels, err = store.ListEndpointDeliveries(ctx, q, "whe_1", 2)
			return
		})
		if len(evs) != 2 || evs[0].ID != "evt_1" || evs[1].ID != "evt_2" {
			t.Fatalf("events %+v", evs)
		}
		if len(dels) != 2 || dels[0].DeliveryID != "whd_1" || dels[0].EventType != "payment.succeeded" || dels[0].EndpointURL != "https://example.com/h" ||
			dels[0].Status != "delivered" || dels[1].DeliveryID != "whd_2" || dels[1].Status != "failed" || dels[1].LastStatus != 500 || dels[1].LastError != "http 500" {
			t.Fatalf("deliveries %+v", dels)
		}
		if len(epDels) != 2 || epDels[0].DeliveryID != "whd_3" || epDels[1].DeliveryID != "whd_2" {
			t.Fatalf("endpoint deliveries (newest first, limited): %+v", epDels)
		}
	})
}

func TestListObjectEvents(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		err := db.Write(ctx, func(tx store.Tx) error {
			for _, e := range []store.EventRow{
				{ID: "evt_2", OrgID: store.DefaultOrgID, Type: "dispute.updated", ObjectType: "dispute", ObjectID: "dsp_1", CreatedAt: t0.Add(time.Second)},
				{ID: "evt_1", OrgID: store.DefaultOrgID, Type: "dispute.opened", ObjectType: "dispute", ObjectID: "dsp_1", CreatedAt: t0},
				{ID: "evt_3", OrgID: store.DefaultOrgID, Type: "dispute.opened", ObjectType: "dispute", ObjectID: "dsp_2", CreatedAt: t0},
				{ID: "evt_4", OrgID: "org_other", Type: "dispute.opened", ObjectType: "dispute", ObjectID: "dsp_1", CreatedAt: t0},
			} {
				e.AppID, e.Mode, e.ObjectVersion, e.Data = "app_a", "live", 1, json.RawMessage(`{}`)
				if err := db.InsertEvent(ctx, tx, e); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var evs []store.EventRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			evs, err = store.ListObjectEvents(ctx, q, store.DefaultOrgID, "dispute", "dsp_1")
			return
		})
		if len(evs) != 2 || evs[0].ID != "evt_1" || evs[1].ID != "evt_2" {
			t.Fatalf("%+v", evs)
		}
	})
}

func seedSeries(t *testing.T, db *store.DB, entries []store.LedgerEntry) {
	t.Helper()
	err := db.Write(ctx, func(tx store.Tx) error {
		for _, p := range []store.Payment{
			pay("pay_1", "app_a", "test", "succeeded", "", "", t0), pay("pay_2", "app_b", "test", "succeeded", "", "", t0),
			pay("pay_3", "app_a", "live", "succeeded", "", "", t0),
		} {
			if err := store.InsertPayment(ctx, tx, p); err != nil {
				return err
			}
		}
		for i, e := range entries {
			e.ID, e.OrgID = "led_"+string(rune('a'+i)), store.DefaultOrgID
			if err := store.InsertLedgerEntry(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLedgerSeriesDaily(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		seedSeries(t, db, []store.LedgerEntry{
			{Kind: "charge", Amount: 10000, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0},
			{Kind: "charge", Amount: 5000, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(time.Hour)},
			{Kind: "refund", Amount: -3000, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(2 * time.Hour)},
			{Kind: "charge", Amount: 700, Currency: "INR", PaymentID: "pay_2", Mode: "test", OccurredAt: t0.Add(25 * time.Hour)},
			{Kind: "charge", Amount: 20, Currency: "USD", PaymentID: "pay_2", Mode: "test", OccurredAt: t0.Add(25 * time.Hour)},
			{Kind: "charge", Amount: 99, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(-48 * time.Hour)}, // before From
			{Kind: "charge", Amount: 98, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(72 * time.Hour)},  // at/after To
			{Kind: "fee", Amount: -50, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0},
			{Kind: "charge", Amount: 1234, Currency: "INR", PaymentID: "pay_3", Mode: "live", OccurredAt: t0},
		})
		var got []store.SeriesRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			got, err = store.LedgerSeries(ctx, q, store.SeriesFilter{OrgID: store.DefaultOrgID, Mode: "test",
				From: t0.Add(-time.Hour), To: t0.Add(72 * time.Hour), OffsetSeconds: 19800})
			return
		})
		day := t0.Add(19800*time.Second).UnixMilli() / 86_400_000
		want := []store.SeriesRow{
			{Slot: day, Currency: "INR", Gross: 15000, Refunds: 3000},
			{Slot: day + 1, Currency: "INR", Gross: 700},
			{Slot: day + 1, Currency: "USD", Gross: 20},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v want %+v", got, want)
		}
		// another org sees nothing
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			got, err = store.LedgerSeries(ctx, q, store.SeriesFilter{OrgID: "org_other", Mode: "test", From: t0.Add(-time.Hour), To: t0.Add(72 * time.Hour)})
			return
		})
		if len(got) != 0 {
			t.Fatalf("other org: %+v", got)
		}
	})
}

func TestLedgerSeriesHourlyAndApp(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		seedSeries(t, db, []store.LedgerEntry{
			{Kind: "charge", Amount: 100, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0},
			{Kind: "charge", Amount: 200, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(10 * time.Minute)},
			{Kind: "refund", Amount: -50, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(time.Hour)},
			{Kind: "charge", Amount: 999, Currency: "INR", PaymentID: "pay_2", Mode: "test", OccurredAt: t0}, // other app
		})
		var got []store.SeriesRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			got, err = store.LedgerSeries(ctx, q, store.SeriesFilter{OrgID: store.DefaultOrgID, Mode: "test", AppID: "app_a",
				From: t0, To: t0.Add(3 * time.Hour), Hourly: true, OffsetSeconds: 19800})
			return
		})
		h := t0.Add(19800*time.Second).UnixMilli() / 3_600_000
		want := []store.SeriesRow{
			{Slot: h, Currency: "INR", Gross: 300},
			{Slot: h + 1, Currency: "INR", Refunds: 50},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v want %+v", got, want)
		}
	})
}

func TestFirstLedgerAt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		seedBase(t, db)
		first := func(mode string) (at time.Time, ok bool) {
			_ = db.Read(ctx, func(q store.Querier) (err error) {
				at, ok, err = store.FirstLedgerAt(ctx, q, store.DefaultOrgID, mode)
				return
			})
			return
		}
		if _, ok := first("test"); ok {
			t.Fatal("empty ledger reported an entry")
		}
		seedSeries(t, db, []store.LedgerEntry{
			{Kind: "charge", Amount: 1, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(time.Hour)},
			{Kind: "charge", Amount: 1, Currency: "INR", PaymentID: "pay_1", Mode: "test", OccurredAt: t0.Add(-time.Hour)},
			{Kind: "charge", Amount: 1, Currency: "INR", PaymentID: "pay_3", Mode: "live", OccurredAt: t0.Add(-10 * time.Hour)},
		})
		if at, ok := first("test"); !ok || !at.Equal(t0.Add(-time.Hour)) {
			t.Fatalf("got %v %v", at, ok)
		}
		if _, ok := first("sandbox"); ok {
			t.Fatal("unknown mode reported an entry")
		}
	})
}
