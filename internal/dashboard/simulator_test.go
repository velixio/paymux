package dashboard

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/connectors/simulator"
	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

// simPayment makes a captured payment on a test-mode simulator account, as
// the simulator knows it and as Paymux stores it.
func (e *env) simPayment() (store.Payment, store.Attempt, simulator.Controller) {
	e.t.Helper()
	app := e.seedApp("sim")
	pa, err := e.conn.CreateAccount(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, connectors.CreateAccountInput{
		Connector: simulator.Name, Name: "Simulator", Mode: "test", Config: json.RawMessage(`{}`), AppID: app.ID})
	if err != nil {
		e.t.Fatal(err)
	}
	live, err := e.conn.Get(ctx, pa.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	ctl := live.Connector.(simulator.Controller)
	if _, err := live.Connector.CreateCheckout(ctx, connector.CheckoutRequest{ProviderRef: "SIMREF0000000000001", Amount: 10000, Currency: "INR"}); err != nil {
		e.t.Fatal(err)
	}
	if err := ctl.Decide(ctx, "SIMREF0000000000001", simulator.ActionSucceed, 0); err != nil {
		e.t.Fatal(err)
	}
	snap, err := live.Connector.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "SIMREF0000000000001"})
	if err != nil {
		e.t.Fatal(err)
	}
	p := e.seedPayment("pay_sim1", app.ID, "test", "succeeded", "ORD-SIM", 10000, e.clk.Now())
	att := store.Attempt{ID: "att_sim1", Mode: "test", PaymentID: p.ID, ProviderAccountID: pa.ID, ProviderRef: "SIMREF0000000000001", ProviderOrderID: snap.ProviderOrderID,
		ProviderPaymentID: snap.ProviderPaymentID, Status: "captured", Method: "simulated", AmountReceived: 10000, CurrencyReceived: "INR"}
	e.seedAttempt(att)
	return p, att, ctl
}

func TestSimulateDisputeFromTheDashboard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, att, ctl := e.simPayment()
		page := "/dashboard/payments/" + p.ID
		path := page + "/simulate-dispute"

		op := e.roleClient("operator")
		body := e.pageOK(op, page)
		has(t, body, "Simulator", "Simulate a dispute", att.ID)
		tok := csrfOf(t, body)

		viewer := e.roleClient("viewer")
		if r, _ := e.post(viewer, path, url.Values{"attempt_id": {att.ID}, "csrf": {csrfOf(t, e.pageOK(viewer, page))}}); r.StatusCode != 403 {
			t.Fatalf("viewer: %d", r.StatusCode)
		}
		// Not a simulator attempt, or not this payment's: refused.
		if r, _ := e.post(op, path, url.Values{"attempt_id": {"att_nope"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("unknown attempt: %d", r.StatusCode)
		}

		r, out := e.post(op, path, url.Values{"attempt_id": {att.ID}, "reason": {"product_not_received"}, "csrf": {tok}})
		if r.StatusCode != 200 || !strings.Contains(out, "dispute") {
			t.Fatalf("simulate: %d\n%s", r.StatusCode, out)
		}
		pg, err := ctl.(connector.DisputeReader).ListDisputes(ctx, connector.TimeRange{From: 0}, "")
		if err != nil || len(pg.Items) != 1 || pg.Items[0].Amount != 10000 || pg.Items[0].Reason != "product_not_received" || pg.Items[0].ProviderPaymentID != att.ProviderPaymentID {
			t.Fatalf("the simulator's disputes: %+v %v", pg, err)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "simulator.dispute", 10)
			return
		})
		if len(aud) != 1 || aud[0].Target != p.ID {
			t.Errorf("audit %+v", aud)
		}
	})
}

func TestSimulatorBoxIsOnlyForSimulatorPayments(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		f := e.detailFixture() // a live Razorpay payment
		op := e.roleClient("operator")
		body := e.pageOK(op, "/dashboard/payments/"+f.pay.ID)
		lacks(t, body, "Simulate a dispute")
		tok := csrfOf(t, body)
		if r, _ := e.post(op, "/dashboard/payments/"+f.pay.ID+"/simulate-dispute", url.Values{"attempt_id": {"att_ok"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("simulating a dispute on a real provider: %d", r.StatusCode)
		}
	})
}

func TestSimulateRenewalFromTheDashboard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.enableSubscriptions()
		app := e.seedApp("simsub")
		e.seedCustomer("cus_s", app.ID, "test", "s@example.com")
		pa, err := e.conn.CreateAccount(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, connectors.CreateAccountInput{
			Connector: simulator.Name, Name: "Simulator", Mode: "test", Config: json.RawMessage(`{}`), AppID: app.ID})
		if err != nil {
			t.Fatal(err)
		}
		live, err := e.conn.Get(ctx, pa.ID)
		if err != nil {
			t.Fatal(err)
		}
		ctl := live.Connector.(simulator.Controller)
		res, err := live.Connector.(connector.SubscriptionManager).CreateSubscriptionCheckout(ctx, connector.SubscriptionRequest{ProviderRef: "SUBREF0000000000001", Amount: 49900, Currency: "INR", Interval: "month", IntervalCount: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := ctl.Decide(ctx, "SUBREF0000000000001", simulator.ActionSucceed, 0); err != nil {
			t.Fatal(err)
		}
		sub := e.seedSub("sub_sim", app.ID, "cus_s", "test", "active", func(s *store.Subscription) {
			s.BillingMode, s.ProviderAccountID, s.ProviderSubscriptionID = "provider_managed", pa.ID, res.ProviderOrderID
		})
		charges := func() int {
			pg, err := live.Connector.(connector.Lister).ListPayments(ctx, connector.TimeRange{From: 0}, "")
			if err != nil {
				t.Fatal(err)
			}
			return len(pg.Items)
		}
		before := charges()
		page := "/dashboard/subscriptions/" + sub.ID
		path := page + "/simulate-renewal"

		op := e.roleClient("operator")
		body := e.pageOK(op, page)
		has(t, body, "Simulate a renewal")
		tok := csrfOf(t, body)
		viewer := e.roleClient("viewer")
		if r, _ := e.post(viewer, path, url.Values{"csrf": {csrfOf(t, e.pageOK(viewer, page))}}); r.StatusCode != 403 {
			t.Fatalf("viewer: %d", r.StatusCode)
		}
		if r, out := e.post(op, path, url.Values{"csrf": {tok}}); r.StatusCode != 200 && r.StatusCode != 303 {
			t.Fatalf("renewal: %d\n%s", r.StatusCode, out)
		}
		if got := charges(); got != before+1 {
			t.Errorf("payments at the provider: %d, want %d", got, before+1)
		}
		if r, _ := e.post(op, path, url.Values{"fail": {"1"}, "csrf": {tok}}); r.StatusCode != 200 && r.StatusCode != 303 {
			t.Fatalf("failed renewal: %d", r.StatusCode)
		}
		if s, err := live.Connector.(connector.SubscriptionManager).FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: res.ProviderOrderID}); err != nil || s.Status != "past_due" {
			t.Errorf("after a failed renewal: %+v %v", s, err)
		}

		// A mandate subscription, or a real provider's, has no such button.
		other := e.seedSub("sub_real", app.ID, "cus_s", "test", "active", func(s *store.Subscription) { s.BillingMode = "provider_managed" })
		lacks(t, e.pageOK(op, "/dashboard/subscriptions/"+other.ID), "Simulate a renewal")
		if r, _ := e.post(op, "/dashboard/subscriptions/"+other.ID+"/simulate-renewal", url.Values{"csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("simulating on a real provider: %d", r.StatusCode)
		}
	})
}

func TestSimulateAProviderDashboardRefund(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, att, ctl := e.simPayment()
		page := "/dashboard/payments/" + p.ID
		path := page + "/simulate-refund"

		op := e.roleClient("operator")
		body := e.pageOK(op, page)
		has(t, body, "Refund from the provider", att.ID)
		tok := csrfOf(t, body)
		viewer := e.roleClient("viewer")
		if r, _ := e.post(viewer, path, url.Values{"attempt_id": {att.ID}, "csrf": {csrfOf(t, e.pageOK(viewer, page))}}); r.StatusCode != 403 {
			t.Fatalf("viewer: %d", r.StatusCode)
		}
		if r, _ := e.post(op, path, url.Values{"attempt_id": {att.ID}, "amount": {"-3"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("a negative amount: %d", r.StatusCode)
		}
		if r, _ := e.post(op, path, url.Values{"attempt_id": {"att_nope"}, "csrf": {tok}}); r.StatusCode != 400 {
			t.Errorf("unknown attempt: %d", r.StatusCode)
		}
		r, out := e.post(op, path, url.Values{"attempt_id": {att.ID}, "amount": {"25.50"}, "csrf": {tok}})
		if r.StatusCode != 200 || !strings.Contains(out, "Refund made at the simulated provider") {
			t.Fatalf("refund: %d\n%s", r.StatusCode, out)
		}
		pg, err := ctl.(connector.Lister).ListRefunds(ctx, connector.TimeRange{From: 0}, "")
		if err != nil || len(pg.Items) != 1 || pg.Items[0].Amount != 2550 || pg.Items[0].ProviderPaymentID != att.ProviderPaymentID || pg.Items[0].ProviderRef != "" {
			t.Fatalf("the simulator's refunds: %+v %v", pg, err)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "simulator.provider_refund", 10)
			return
		})
		if len(aud) != 1 || aud[0].Target != p.ID {
			t.Errorf("audit %+v", aud)
		}
		// Nothing was booked in Paymux by the click itself: that comes from the webhook.
		if e.count("SELECT COUNT(*) FROM refunds") != 0 {
			t.Error("the click booked a refund directly")
		}
	})
}
