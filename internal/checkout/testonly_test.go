package checkout_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/velixio/paymux/connectors/simulator"
	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
)

// A test-mode simulator account takes the payments no rule routes anywhere, so
// "link the simulator" is all a developer has to do. Rules still win.
func TestTestOnlyConnectorIsTheFallbackInTestMode(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// Everything is routed to a connector that has no account here.
		e.routeFake(`{"name":"razorpay-only","when":{},"action":{"kind":"route","targets":[{"connector":"razorpay"}]}}`)
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR"}); !errors.Is(err, core.ErrNoRoute) {
			t.Fatalf("without a simulator the payment has no route: %v", err)
		}

		sim, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: simulator.Name, Name: "sim", Mode: "test",
			Config: json.RawMessage(`{}`), AppID: "app_1"})
		if err != nil {
			t.Fatal(err)
		}
		p := e.payment(100, "INR")
		st, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.AccountID != sim.ID || st.Connector != simulator.Name || st.Action.Kind != "redirect" {
			t.Errorf("checkout went to %s (%s) %+v, want the simulator %s", st.AccountID, st.Connector, st.Action, sim.ID)
		}

		// A currency the account does not accept is still no route.
		inr := []string{"INR"}
		if err := e.conns.UpdateAccount(ctx, audit.System, org, sim.ID, connectors.UpdateAccountInput{Currencies: inr}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "USD"}); !errors.Is(err, core.ErrNoRoute) {
			t.Errorf("USD on an INR-only simulator account: %v", err)
		}
		// An explicit reject rule is a decision, not a gap: no fallback.
		e.routeFake(`{"name":"no-inr","when":{"currency":["INR"]},"action":{"kind":"reject","message":"INR is not accepted here"}}`)
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR"}); !errors.Is(err, core.ErrCurrencyNotAccepted) {
			t.Errorf("a reject rule must stand: %v", err)
		}
		// A rule that names the simulator, or an account that serves it, needs no fallback.
		e.routeFake(`{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"` + e.fake.Name + `"}]}}`)
		p = e.payment(100, "INR")
		if st, err := e.flow.StartCheckout(ctx, p.ID); err != nil || st.Connector != e.fake.Name {
			t.Errorf("rules that match keep working: %v %+v", err, st)
		}
		_ = store.DefaultOrgID
	})
}
