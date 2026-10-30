package checkout_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// A payment's routing restrictions (allowed_connectors, country) are part of
// the payment: every routing decision for it, at the pay page, on a retry, on
// "pay another way" and on a fallback, must honour them, not only the one made
// when the payment was created.

type twoProviders struct {
	x, y     *fakeconn.State // two connectors; the rules put Y first
	xa, ya   store.ProviderAccount
	attempts func(paymentID string) []store.Attempt
}

func (e *env) twoProviders() twoProviders {
	e.t.Helper()
	y := fakeconn.Register("yfirst")
	mk := func(st *fakeconn.State, name string) store.ProviderAccount {
		pa, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: st.Name, Name: name, Mode: "test",
			Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1"})
		if err != nil {
			e.t.Fatal(err)
		}
		return pa
	}
	tp := twoProviders{x: e.fake, y: y, xa: mk(e.fake, "x"), ya: mk(y, "y"), attempts: e.attempts}
	return tp
}

func (e *env) routeRules(rules, catchAll string) {
	e.t.Helper()
	doc := `{"rules":[` + rules + `],"catch_all":` + catchAll + `}`
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(doc)); err != nil {
		e.t.Fatal(err)
	}
}

func route(name, when string, connectors ...string) string {
	t := ""
	for i, c := range connectors {
		if i > 0 {
			t += ","
		}
		t += `{"connector":"` + c + `"}`
	}
	return `{"name":"` + name + `","when":` + when + `,"action":{"kind":"route","targets":[` + t + `]}}`
}

func checkoutsAsked(st *fakeconn.State) (n int) {
	st.Locked(func() { n = len(st.Checkouts) })
	return
}

func accountsOf(atts []store.Attempt) []string {
	var out []string
	for _, a := range atts {
		out = append(out, a.ProviderAccountID)
	}
	return out
}

func TestAllowedConnectorsAreKeptForTheWholeLifeOfThePayment(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		tp := e.twoProviders()
		// The catch-all prefers Y; the app allows only X.
		e.routeRules("", route("all", `{}`, tp.y.Name, tp.x.Name))
		res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR", AllowedConnectors: []string{tp.x.Name}})
		if err != nil {
			t.Fatal(err)
		}
		if got := e.get(res.Payment.ID); len(got.AllowedConnectors) != 1 || got.AllowedConnectors[0] != tp.x.Name {
			t.Fatalf("the restriction is not kept on the payment: %v", got.AllowedConnectors)
		}
		// The pay page.
		if _, err := e.flow.StartCheckout(ctx, res.Payment.ID); err != nil {
			t.Fatal(err)
		}
		// "Try again" and "pay another way".
		if _, err := e.flow.StartFreshCheckout(ctx, res.Payment.ID); err != nil {
			t.Fatal(err)
		}
		for _, a := range e.attempts(res.Payment.ID) {
			if a.ProviderAccountID == tp.ya.ID {
				t.Errorf("an attempt went to %s, outside allowed_connectors [%s]", a.ProviderAccountID, tp.x.Name)
			}
		}
		if len(e.attempts(res.Payment.ID)) < 2 {
			t.Errorf("attempts = %v", accountsOf(e.attempts(res.Payment.ID)))
		}
		if n := checkoutsAsked(tp.y); n != 0 {
			t.Errorf("the excluded provider was asked for %d checkouts", n)
		}
	})
}

// A fallback after a failure stays inside the allowed connectors too: with the
// only allowed provider down, the payment fails rather than reaching another.
func TestFallbackNeverLeavesTheAllowedConnectors(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		tp := e.twoProviders()
		e.routeRules("", route("all", `{}`, tp.x.Name, tp.y.Name))
		tp.x.Locked(func() {
			tp.x.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{}, connector.ErrTemporary
			}
		})
		res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR", AllowedConnectors: []string{tp.x.Name}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.flow.StartCheckout(ctx, res.Payment.ID); err == nil {
			t.Error("the checkout succeeded though the only allowed provider failed")
		}
		for _, a := range e.attempts(res.Payment.ID) {
			if a.ProviderAccountID == tp.ya.ID {
				t.Error("fell back to a provider the app excluded")
			}
		}
		if n := checkoutsAsked(tp.y); n != 0 {
			t.Errorf("the excluded provider was asked for %d checkouts", n)
		}
	})
}

// Routing rules can match on the buyer's country; the country given when the
// payment was created decides at the pay page as well.
func TestCountryOfThePaymentIsRoutedOnAtThePayPage(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		tp := e.twoProviders()
		e.routeRules(route("us", `{"country":["US"]}`, tp.y.Name), route("all", `{}`, tp.x.Name))
		us, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR", Country: "US"})
		if err != nil {
			t.Fatal(err)
		}
		other, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR", Country: "IN"})
		if err != nil {
			t.Fatal(err)
		}
		if got := e.get(us.Payment.ID); got.Country != "US" {
			t.Fatalf("country kept = %q", got.Country)
		}
		for id, wantY := range map[string]bool{us.Payment.ID: true, other.Payment.ID: false} {
			if _, err := e.flow.StartCheckout(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, err := e.flow.StartFreshCheckout(ctx, id); err != nil {
				t.Fatal(err)
			}
			for _, a := range e.attempts(id) {
				if (a.ProviderAccountID == tp.ya.ID) != wantY {
					t.Errorf("payment %s: attempt on %s (Y account %s), want Y=%v", id, a.ProviderAccountID, tp.ya.ID, wantY)
				}
			}
		}
	})
}

// A restriction that no provider can meet is refused when the payment is
// created (as before), not left to fail at the pay page.
func TestAnAllowedConnectorWithNoRouteIsRefusedAtCreate(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		tp := e.twoProviders()
		e.routeRules("", route("all", `{}`, tp.x.Name))
		_, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR", AllowedConnectors: []string{tp.y.Name}})
		if !errors.Is(err, core.ErrNoRoute) {
			t.Errorf("err = %v", err)
		}
	})
}
