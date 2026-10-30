package checkout_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/pkg/connector"
)

// alertLog is the ops alert sender of a test.
type alertLog struct {
	mu  sync.Mutex
	got []notify.Alert
}

func (a *alertLog) Raise(_ context.Context, al notify.Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, al)
	return nil
}

func (a *alertLog) all() []notify.Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]notify.Alert(nil), a.got...)
}

// offSite is a next action to a host the fake connector never declared, one
// per kind of action.
var offSite = map[string]connector.NextAction{
	"redirect":  {Kind: connector.ActionRedirect, URL: "https://evil.example.org/pay"},
	"form_post": {Kind: connector.ActionFormPost, URL: "https://evil.example.org/post", Fields: map[string]string{"a": "b"}},
	"sdk":       {Kind: connector.ActionSDK, ScriptURL: "https://evil.example.org/sdk.js", Options: map[string]any{"k": "v"}},
	"http":      {Kind: connector.ActionRedirect, URL: "http://fake.example.com/not-https"},
}

// A connector that answers with a URL outside its declared hosts gets no
// checkout: the attempt fails, an ops alert names the account, and routing
// goes on with the next account.
func TestCheckoutOutsideDeclaredHostsFallsBack(t *testing.T) {
	for name, bad := range offSite {
		t.Run(name, func(t *testing.T) {
			each(t, func(t *testing.T, e *env) {
				p := e.payment2()
				calls := 0
				e.fake.Locked(func() {
					e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
						calls++
						if calls == 1 {
							return connector.CheckoutResult{ProviderOrderID: "o1", NextAction: bad}, nil
						}
						return connector.CheckoutResult{ProviderOrderID: "o2", NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: "https://x.example.com/p"}}, nil
					}
				})
				st, err := e.flow.StartCheckout(ctx, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if st.Action.URL != "https://x.example.com/p" {
					t.Fatalf("the customer was sent to %+v", st.Action)
				}
				var failed, created int
				var failedAccount string
				for _, a := range e.attempts(p.ID) {
					switch a.Status {
					case "failed":
						failed++
						failedAccount = a.ProviderAccountID
						if a.FailureCode != "checkout_error" || !strings.Contains(a.FailureMessage, "declared checkout hosts") && !strings.Contains(a.FailureMessage, "not https") {
							t.Errorf("failure = %s %q", a.FailureCode, a.FailureMessage)
						}
					case "created":
						created++
						if a.ProviderAccountID == failedAccount && failedAccount != "" {
							t.Error("the fallback must use the next account")
						}
					}
				}
				if failed != 1 || created != 1 {
					t.Fatalf("attempts: %d failed, %d created", failed, created)
				}
				al := e.alerts.all()
				if len(al) != 1 || al[0].Kind != "provider.checkout_host_refused" || al[0].Refs["provider_account"] != failedAccount ||
					!strings.Contains(al[0].Message, failedAccount) {
					t.Fatalf("alerts = %+v (failed account %s)", al, failedAccount)
				}
				if got := e.get(p.ID); got.Status != "created" {
					t.Errorf("payment = %s", got.Status)
				}
			})
		})
	}
}

// When every account answers off-site the payment gets no checkout at all.
func TestCheckoutOutsideDeclaredHostsOnEveryAccount(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment2()
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{ProviderOrderID: "o", NextAction: offSite["redirect"]}, nil
			}
		})
		_, err := e.flow.StartCheckout(ctx, p.ID)
		if !errors.Is(err, checkout.ErrCheckoutFailed) {
			t.Fatalf("err = %v", err)
		}
		if n := len(e.alerts.all()); n != 2 {
			t.Errorf("alerts = %d, want one per refused account", n)
		}
	})
}

// A connector with no declared hosts at all may not send the customer
// anywhere.
func TestCheckoutFromAConnectorWithoutHostsIsRefused(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment2()
		e.fake.Locked(func() { e.fake.NoHosts = true })
		if _, err := e.flow.StartCheckout(ctx, p.ID); !errors.Is(err, checkout.ErrCheckoutFailed) {
			t.Fatalf("err = %v", err)
		}
	})
}

// A next action stored before the account's hosts changed (or before the hosts
// were enforced) is not shown again: the attempt is not reusable, and a fresh
// checkout is made, which is checked like any other.
func TestStoredActionOutsideTheHostsIsNotReused(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.sdkCheckouts(nil)
		p := e.payment2()
		first, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The connector's declaration no longer covers the stored action.
		e.fake.Locked(func() { e.fake.NoHosts = true })
		if st, err := e.flow.StartCheckout(ctx, p.ID); !errors.Is(err, checkout.ErrCheckoutFailed) {
			t.Fatalf("the stored action was shown again: %+v %v", st.Action, err)
		}
		if n := e.checkoutCalls(); n < 2 {
			t.Errorf("%d provider checkouts: the stored one was reused without a check", n)
		}
		if len(e.alerts.all()) == 0 {
			t.Error("no alert for the refused stored action")
		}
		// With the hosts declared again the payment can be paid (the first attempt
		// is open and its action is declared again, so it may be shown).
		e.fake.Locked(func() { e.fake.NoHosts = false })
		if _, err := e.flow.StartCheckout(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		_ = first
	})
}
