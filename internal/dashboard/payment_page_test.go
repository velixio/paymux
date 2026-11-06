package dashboard

import (
	"testing"

	"github.com/velixio/paymux/internal/outbound"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

func TestPaymentPageShowsTimelineNoHeadingsForTabs(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/payments/"+f.pay.ID)
		has(t, body, `class="payment-timeline"`, "Last failure", "Tries", "Risk", "Webhooks", "Received",
			"Checkout started", "Provider IDs and logs", "All events", "Asha Rao", "10 Market Road")
		lacks(t, body, "<h3")
	})
}

func TestPaymentPageEscapesProviderText(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		p := e.seedPayment("pay_x", app.ID, "live", "failed", "ORD-X", 100, e.clk.Now())
		const evil = `<img src=x onerror=alert(1)>`
		e.seedAttempt(store.Attempt{ID: "att_x", Mode: "live", PaymentID: p.ID, ProviderAccountID: "pa_x", ProviderRef: "pr_x", Status: "failed",
			Method: "card", FailureMessage: evil, MethodDetails: `{"card":{"name":"<img src=x onerror=alert(1)>"}}`})
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/payments/"+p.ID)
		lacks(t, body, evil)
		has(t, body, "&lt;img src=x")
	})
}

// failDelivery leaves the fixture's delivery failed, so it can be retried.
func failDelivery(t *testing.T, e *env) {
	t.Helper()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE webhook_deliveries SET status = 'failed', last_status = 500`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPaymentPageKeepsActions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		failDelivery(t, e)
		body := e.pageOK(e.roleClient("operator"), "/dashboard/payments/"+f.pay.ID)
		tok := csrfOf(t, body)
		// The sync form only shows with a syncer: see the sync test in payments_test.go.
		has(t, body, tok, `action="/dashboard/payments/pay_d1/refund"`,
			`action="/dashboard/payments/pay_d1/webhook"`, `action="/dashboard/payments/pay_d1/webhooks/replay"`,
			`/replay" `, "Redeliver to all (1)", "Retry", `command="show-modal"`, `name="nonce"`)
	})
}

func TestPaymentPagePermissions(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		failDelivery(t, e)
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/payments/"+f.pay.ID)
		has(t, body, "500")
		lacks(t, body, "Redeliver to all", ">Retry<", "/replay", "Send status webhook", `/refund"`, "Test tools", `name="nonce"`)
	})
}

// An endpoint the event never reached is no business of someone who cannot send.
func TestPaymentPageHidesNewEndpointsFromViewers(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture()
		if _, _, err := e.out.CreateEndpoint(ctx, outbound.CreateEndpointInput{AppID: f.app.ID, URL: "https://added.example.com/hook"}); err != nil {
			t.Fatal(err)
		}
		lacks(t, e.pageOK(e.roleClient("viewer"), "/dashboard/payments/"+f.pay.ID), "added.example.com")
		has(t, e.pageOK(e.roleClient("operator"), "/dashboard/payments/"+f.pay.ID), "added.example.com", "Not sent", "Redeliver to all (2)")
	})
}

func TestPaymentPageSparse(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		p := e.seedPayment("pay_s", app.ID, "live", "failed", "ORD-S", 100, e.clk.Now())
		e.seedAttempt(store.Attempt{ID: "att_s", Mode: "live", PaymentID: p.ID, ProviderAccountID: "pa_x", ProviderRef: "pr_s", Status: "failed", Method: "card", FailureCode: "failed"})
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/payments/"+p.ID)
		has(t, body, "No reason given", "Not provided", "Payment failed", "1 failed", ">Card<")
		lacks(t, body, `pt-risk"`, "3D Secure", "No match")
	})
}

func TestPaymentPageNoTries(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app := e.seedApp("alpha")
		p := e.seedPayment("pay_n", app.ID, "live", "requires_payment_method", "ORD-N", 100, e.clk.Now())
		body := e.pageOK(e.roleClient("viewer"), "/dashboard/payments/"+p.ID)
		has(t, body, "The customer hasn't tried to pay yet.", "None yet", "Not provided")
		lacks(t, body, "✕")
	})
}
