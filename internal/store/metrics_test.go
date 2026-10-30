package store_test

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestBusinessCounts(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		got, err := db.BusinessCounts(ctx)
		if err != nil || len(got.Payments) != 0 || got.InvoicesIssued != 0 || got.WebhooksFailed != 0 || len(got.Subscriptions) != 0 {
			t.Fatalf("empty db: %+v %v", got, err)
		}

		err = db.Write(ctx, func(tx store.Tx) error {
			for _, a := range []store.ProviderAccount{
				{ID: "pa_rzp", Connector: "razorpay"}, {ID: "pa_payu", Connector: "payu"},
			} {
				a.OrgID, a.Mode, a.Name, a.Status, a.CreatedAt = store.DefaultOrgID, "live", a.ID, "active", t0
				if err := store.InsertProviderAccount(ctx, tx, a); err != nil {
					return err
				}
			}
			pay := func(id, mode, status string) error {
				return store.InsertPayment(ctx, tx, store.Payment{ID: id, OrgID: "o", AppID: "app_1", Mode: mode, Kind: "one_time", Amount: 100,
					Currency: "INR", Status: status, ClientSecretHash: "x", ExpiresAt: t0, Version: 1, CreatedAt: t0, UpdatedAt: t0})
			}
			att := func(id, payment, account string, at time.Time) error {
				return store.InsertAttempt(ctx, tx, store.Attempt{ID: id, OrgID: "o", Mode: "live", PaymentID: payment, ProviderAccountID: account,
					ProviderRef: "ref_" + id, Status: "created", CreatedAt: at, UpdatedAt: at})
			}
			for _, p := range [][3]string{{"p1", "live", "succeeded"}, {"p2", "live", "succeeded"}, {"p3", "live", "failed"}, {"p4", "test", "created"}} {
				if err := pay(p[0], p[1], p[2]); err != nil {
					return err
				}
			}
			// p1 failed on razorpay first and succeeded on payu: the latest attempt counts.
			for _, a := range []struct {
				id, pay, acc string
				at           time.Time
			}{{"a1", "p1", "pa_rzp", t0}, {"a2", "p1", "pa_payu", t0.Add(time.Minute)}, {"a3", "p2", "pa_rzp", t0}, {"a4", "p3", "pa_rzp", t0}} {
				if err := att(a.id, a.pay, a.acc, a.at); err != nil {
					return err
				}
			}
			for i, st := range []string{"active", "active", "past_due"} {
				s := store.Subscription{ID: "sub_" + string(rune('a'+i)), OrgID: "o", Mode: "live", AppID: "app_1", CustomerID: "c", Status: st,
					BillingMode: "merchant_triggered", ProviderAccountID: "pa_payu", ProviderRef: "SUB" + string(rune('A'+i)), Amount: 100, Currency: "INR",
					Interval: "month", IntervalCount: 1, AnchorAt: t0, CurrentPeriodStart: t0, CurrentPeriodEnd: t0, CheckoutTokenHash: "h", ExpiresAt: t0,
					Version: 1, CreatedAt: t0, UpdatedAt: t0}
				if err := store.InsertSubscription(ctx, tx, s); err != nil {
					return err
				}
			}
			for i := 0; i < 2; i++ {
				id := string(rune('a' + i))
				if _, err := tx.ExecContext(ctx, `INSERT INTO invoices (id, org_id, mode, app_id, tax_entity_id, payment_id, attempt_id, number, series_prefix,
					fiscal_year, issued_at, supply_type, place_of_supply, currency, supplier, recipient, lines, taxable, total, template_version, access_token_hash)
					VALUES (?, 'o', 'live', 'app_1', 'te_1', 'p1', 'a1', ?, 'INV', '2627', 1, 'b2c', '27', 'INR', '{}', '{}', '[]', 100, 100, 'v1', 'h')`,
					"inv_"+id, "INV/"+id); err != nil {
					return err
				}
			}
			for i, st := range []string{"failed", "failed", "delivered", "pending"} {
				if err := db.InsertDelivery(ctx, tx, store.DeliveryRow{ID: "d" + string(rune('a'+i)), EventID: "e" + string(rune('a'+i)), EndpointID: "ep", Status: st,
					CreatedAt: t0, UpdatedAt: t0}); err != nil {
					return err
				}
			}
			for i, v := range []bool{false, false, true} {
				if _, err := db.InsertProviderEvent(ctx, tx, store.ProviderEventRow{ID: "pe" + string(rune('a'+i)), ProviderAccountID: "pa_payu",
					ProviderEventID: "x" + string(rune('a'+i)), Headers: []byte(`{}`), Body: []byte(`{}`), Verified: v, ReceivedAt: t0}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		got, err = db.BusinessCounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sort.Slice(got.Payments, func(i, j int) bool {
			a, b := got.Payments[i], got.Payments[j]
			return a.Status+a.Connector+a.Mode < b.Status+b.Connector+b.Mode
		})
		wantPay := []store.PaymentCount{
			{"created", "none", "test", 1}, {"failed", "razorpay", "live", 1}, {"succeeded", "payu", "live", 1}, {"succeeded", "razorpay", "live", 1},
		}
		if !reflect.DeepEqual(got.Payments, wantPay) {
			t.Errorf("payments = %+v", got.Payments)
		}
		if !reflect.DeepEqual(got.Subscriptions, map[string]int64{"active": 2, "past_due": 1}) {
			t.Errorf("subscriptions = %v", got.Subscriptions)
		}
		if got.InvoicesIssued != 2 || got.WebhooksFailed != 2 || !reflect.DeepEqual(got.InboundUnverified, map[string]int64{"payu": 2}) {
			t.Errorf("counts = %+v", got)
		}
	})
}
