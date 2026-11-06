// Command paymux-e2eseed fills a throw-away data directory with payments and
// ledger rows for the dashboard browser tests (frontend/dashboard/e2e). It is
// a separate binary, built only by that test setup: the production binary
// (cmd/paymux) has no flag or command that reaches this code.
//
// Run it after "paymux migrate", "paymux user create" and "paymux app create",
// while no server holds the SQLite lock:
//
//	PAYMUX_DATA_DIR=/tmp/x paymux-e2eseed
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/velixio/paymux/internal/migrations"
	"github.com/velixio/paymux/internal/store"
)

func main() {
	if err := run(context.Background(), os.Getenv("PAYMUX_DATA_DIR")); err != nil {
		fmt.Fprintln(os.Stderr, "paymux-e2eseed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("PAYMUX_DATA_DIR is required")
	}
	db, err := store.Open(ctx, "", dataDir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		return err
	}
	return db.Write(ctx, func(tx store.Tx) error {
		apps, err := store.ListApps(ctx, tx, store.DefaultOrgID)
		if err != nil {
			return err
		}
		if len(apps) == 0 {
			return fmt.Errorf("no app: run \"paymux app create\" first")
		}
		app := apps[0]
		now := time.Now().UTC()
		n := 0
		add := func(at time.Time, mode, cur string, amount, refund int64) error {
			n++
			id := fmt.Sprintf("pay_e2e_%03d", n)
			p := store.Payment{ID: id, OrgID: store.DefaultOrgID, AppID: app.ID, Mode: mode, Kind: "one_time", Amount: amount, AmountReceived: amount,
				Currency: cur, Status: "succeeded", OrderRef: fmt.Sprintf("ORD-%03d", n), Metadata: map[string]string{}, ClientSecretHash: "h",
				ExpiresAt: at.Add(time.Hour), Version: 1, Flags: []string{}, CreatedAt: at, UpdatedAt: at}
			if err := store.InsertPayment(ctx, tx, p); err != nil {
				return err
			}
			entries := []store.LedgerEntry{{Kind: "charge", Amount: amount}}
			if refund > 0 {
				entries = append(entries, store.LedgerEntry{Kind: "refund", Amount: -refund})
			}
			for i, e := range entries {
				e.ID, e.OrgID, e.Mode, e.Currency, e.PaymentID, e.OccurredAt = fmt.Sprintf("led_e2e_%03d_%d", n, i), store.DefaultOrgID, mode, cur, id, at
				if err := store.InsertLedgerEntry(ctx, tx, e); err != nil {
					return err
				}
			}
			return nil
		}
		// Fourteen months back, mid-month, in INR (large) and USD (small), so
		// INR stays the default currency and the 12M view has monthly rows.
		for m := 13; m >= 1; m-- {
			at := time.Date(now.Year(), now.Month()-time.Month(m), 15, 9, 0, 0, 0, time.UTC)
			if err := add(at, "live", "INR", 1_200_000+int64(m)*10_000, int64(m%3)*50_000); err != nil {
				return err
			}
			if err := add(at.Add(2*time.Hour), "live", "USD", 4_000+int64(m)*100, 0); err != nil {
				return err
			}
		}
		// The last few days, so 7D and 30D have data, plus one test-mode payment.
		for d := 0; d <= 5; d++ {
			at := now.Add(-time.Duration(d)*24*time.Hour - time.Hour)
			if err := add(at, "live", "INR", 250_000+int64(d)*5_000, 0); err != nil {
				return err
			}
		}
		if err := add(now.Add(-2*time.Hour), "live", "USD", 9_900, 0); err != nil {
			return err
		}
		// That payment carries a provider's risk review.
		if err := store.InsertProviderAccountInfo(ctx, tx, store.ProviderAccountInfo{ID: "pa_e2e_whop", OrgID: store.DefaultOrgID, Mode: "live",
			Connector: "whop", Name: "Whop", Status: "active", Currencies: []string{"USD"}, Purposes: []string{"one_time"}}, now); err != nil {
			return err
		}
		if err := store.InsertAttempt(ctx, tx, store.Attempt{ID: "att_e2e_risk", OrgID: store.DefaultOrgID, Mode: "live",
			PaymentID: fmt.Sprintf("pay_e2e_%03d", n), ProviderAccountID: "pa_e2e_whop", ProviderRef: "pr_e2e_risk", Status: "captured",
			AmountReceived: 9_900, CurrencyReceived: "USD", CreatedAt: now, UpdatedAt: now,
			RiskDetails: `{"score":19,"ip_address":"203.0.113.24","city":"Testville","region":"Sample State","country":"US","address_check":"no_match","postal_check":"match","cvv_check":"match","three_d_secure":false,"authorization_code":"A1B2C3"}`}); err != nil {
			return err
		}
		if err := add(now.Add(-3*time.Hour), "test", "INR", 77_700, 0); err != nil {
			return err
		}
		// A payment that failed twice, for the payment page.
		at := now.Add(-4 * time.Hour)
		if err := store.InsertPayment(ctx, tx, store.Payment{ID: "pay_e2e_tries", OrgID: store.DefaultOrgID, AppID: app.ID, Mode: "live", Kind: "one_time",
			Amount: 9_900, Currency: "USD", Status: "failed", OrderRef: "ORD-TRIES", Metadata: map[string]string{}, ClientSecretHash: "h",
			ExpiresAt: at.Add(time.Hour), Version: 1, Flags: []string{}, CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		if err := store.InsertAttempt(ctx, tx, store.Attempt{ID: "att_e2e_tries", OrgID: store.DefaultOrgID, Mode: "live", PaymentID: "pay_e2e_tries",
			ProviderAccountID: "pa_e2e_whop", ProviderRef: "pr_e2e_tries", Status: "failed", CreatedAt: at, UpdatedAt: at}); err != nil {
			return err
		}
		if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: "led_e2e_tries", OrgID: store.DefaultOrgID, Mode: "live", Kind: "fee", Amount: 0,
			Currency: "USD", PaymentID: "pay_e2e_tries", OccurredAt: at}); err != nil {
			return err
		}
		for i, try := range []store.ProviderPaymentAttempt{
			{ProviderPaymentID: "pi_e2e", TryID: "ch_e2e_declined", ThreeDS: "passed", FailureCode: "insufficient_funds",
				FailureMessage: "Your card has insufficient funds.", Method: "card",
				MethodDetails: `{"card":{"network":"visa","last4":"4242","issuer":"Sample Bank","name":"S CUSTOMER"}}`,
				RiskDetails:   `{"score":62,"address_check":"no_match","postal_check":"match","cvv_check":"match"}`},
			{ProviderPaymentID: "pi_e2e", TryID: "ch_e2e_3ds", ThreeDS: "failed", FailureCode: "authentication_failed", Method: "card",
				MethodDetails: `{"card":{"network":"mastercard","last4":"8210"}}`,
				RiskDetails:   `{"score":62,"address_check":"no_match","postal_check":"match","cvv_check":"match"}`},
		} {
			when := at.Add(time.Duration(i+1) * time.Minute)
			try.ProviderAccountID, try.AttemptID, try.Status, try.Amount, try.Currency, try.CreatedAt, try.UpdatedAt = "pa_e2e_whop", "att_e2e_tries", "failed", 9_900, "USD", when, when
			if err := store.UpsertProviderPaymentAttempt(ctx, tx, try); err != nil {
				return err
			}
		}
		// Who paid, and where the failure webhook went: one endpoint took it, one
		// is failing, and one was added since. Nothing here can really be sent.
		customerID, err := store.UpsertCustomer(ctx, tx, store.Customer{ID: "cus_e2e_tries", OrgID: store.DefaultOrgID, AppID: app.ID, Mode: "live",
			ExternalRef: "cust-42", Name: "Sample Customer", Email: "customer@example.com", Phone: "+15550100",
			Billing: store.Billing{Line1: "1 Example St", City: "Austin", StateCode: "TX", PostalCode: "78701", Country: "US"}}, at)
		if err != nil {
			return err
		}
		if err := store.SetPaymentCustomer(ctx, tx, "pay_e2e_tries", customerID, at); err != nil {
			return err
		}
		if err := db.InsertEvent(ctx, tx, store.EventRow{ID: "evt_e2e_tries", OrgID: store.DefaultOrgID, AppID: app.ID, Mode: "live", Type: "payment.failed",
			ObjectType: "payment", ObjectID: "pay_e2e_tries", ObjectVersion: 1, Data: []byte(`{}`), CreatedAt: at.Add(3 * time.Minute)}); err != nil {
			return err
		}
		for i, url := range []string{"https://api.example.com/hooks", "https://hooks.example.com/paymux", "https://crm.example.com/in"} {
			endpoint := fmt.Sprintf("ep_e2e_%d", i)
			if err := db.InsertEndpoint(ctx, tx, store.EndpointRow{ID: endpoint, OrgID: store.DefaultOrgID, AppID: app.ID, URL: url, SigningSecretEnc: []byte("x"),
				EventTypes: []string{"payment.failed"}, Enabled: true, Mode: "live", CreatedAt: at, UpdatedAt: at}); err != nil {
				return err
			}
			delivery := store.DeliveryRow{ID: fmt.Sprintf("whd_e2e_%d", i), EventID: "evt_e2e_tries", EndpointID: endpoint, AttemptCount: 1, CreatedAt: at, UpdatedAt: at}
			switch i {
			case 0:
				delivery.Status, delivery.LastStatus, delivery.DeliveredAt = "delivered", 200, at
			case 1:
				delivery.Status, delivery.LastStatus, delivery.LastError = "failed", 500, "http 500"
			default:
				continue
			}
			if err := db.InsertDelivery(ctx, tx, delivery); err != nil {
				return err
			}
		}
		// A provider that sends almost nothing, and a payment nobody has tried.
		for _, p := range []struct{ id, status, order string }{{"pay_e2e_sparse", "failed", "ORD-SPARSE"}, {"pay_e2e_untried", "requires_payment_method", "ORD-UNTRIED"}} {
			if err := store.InsertPayment(ctx, tx, store.Payment{ID: p.id, OrgID: store.DefaultOrgID, AppID: app.ID, Mode: "live", Kind: "one_time",
				Amount: 9_900, Currency: "USD", Status: p.status, OrderRef: p.order, Metadata: map[string]string{}, ClientSecretHash: "h",
				ExpiresAt: at.Add(time.Hour), Version: 1, Flags: []string{}, CreatedAt: at, UpdatedAt: at}); err != nil {
				return err
			}
		}
		return store.InsertAttempt(ctx, tx, store.Attempt{ID: "att_e2e_sparse", OrgID: store.DefaultOrgID, Mode: "live", PaymentID: "pay_e2e_sparse",
			ProviderAccountID: "pa_e2e_whop", ProviderRef: "pr_e2e_sparse", Status: "failed", Method: "card", FailureCode: "failed", CreatedAt: at, UpdatedAt: at})
	})
}
