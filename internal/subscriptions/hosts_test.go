package subscriptions_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/subscriptions"
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

// A subscription or mandate checkout that sends the customer outside the hosts
// its connector declared is refused: nothing is stored for the page, the error
// says why, and an ops alert names the account.
func TestSubscriptionCheckoutOutsideDeclaredHostsIsRefused(t *testing.T) {
	bad := map[string]connector.NextAction{
		"redirect":  {Kind: connector.ActionRedirect, URL: "https://evil.example.org/pay"},
		"form_post": {Kind: connector.ActionFormPost, URL: "https://evil.example.org/si", Fields: map[string]string{"a": "b"}},
		"sdk":       {Kind: connector.ActionSDK, ScriptURL: "https://evil.example.org/c.js", Options: map[string]any{"k": "v"}},
		"http":      {Kind: connector.ActionRedirect, URL: "http://pg.example.com/si"},
	}
	for _, path := range []struct{ name, account string }{{"mandate", "pa_upi"}, {"provider managed", "pa_managed"}} {
		for kind, na := range bad {
			t.Run(path.name+"/"+kind, func(t *testing.T) {
				storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
					e := newEnv(t, db, t0)
					if path.account == "pa_managed" {
						e.routeTo("razorpay")
					}
					c := e.mustCreate(func(i *subscriptions.CreateInput) {
						if path.account == "pa_managed" {
							i.Method = ""
						}
					})
					e.r.mu.Lock()
					e.r.badAction = &na
					e.r.mu.Unlock()
					_, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c))
					if !errors.Is(err, connectors.ErrNextActionRefused) {
						t.Fatalf("err = %v", err)
					}
					if got := e.sub(c.Subscription.ID); got.CheckoutAction != "" {
						t.Errorf("the refused action was stored: %s", got.CheckoutAction)
					}
					al := e.alerts.all()
					if len(al) != 1 || al[0].Kind != "provider.checkout_host_refused" || al[0].Refs["provider_account"] != path.account ||
						!strings.Contains(al[0].Message, path.account) {
						t.Fatalf("alerts = %+v", al)
					}
					// Once the service answers on its hosts again, the page works.
					e.r.mu.Lock()
					e.r.badAction = nil
					e.r.mu.Unlock()
					if co, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c)); err != nil || co.Kind != subscriptions.CheckoutAction {
						t.Fatalf("retry: %+v %v", co, err)
					}
				})
			})
		}
	}
}

// The action stored on the subscription is checked again whenever the page is
// opened: a checkout saved before the connector's hosts changed is refused, not
// shown. It is kept, not recreated (a new provider subscription or mandate per
// load would orphan the old one), so the page works again once the hosts do.
func TestStoredSubscriptionActionOutsideTheHostsIsRefused(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		c := e.mustCreate(nil)
		first, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c))
		if err != nil || first.Action.URL == "" {
			t.Fatalf("first open: %+v %v", first, err)
		}
		e.r.mu.Lock()
		e.r.noHosts = true
		e.r.mu.Unlock()
		if _, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c)); !errors.Is(err, connectors.ErrNextActionRefused) {
			t.Fatalf("the stored action was shown: %v", err)
		}
		if len(e.alerts.all()) != 1 {
			t.Errorf("alerts = %+v", e.alerts.all())
		}
		if got := e.sub(c.Subscription.ID); got.CheckoutAction == "" {
			t.Error("the stored action was cleared")
		}
		e.r.mu.Lock()
		e.r.noHosts = false
		e.r.mu.Unlock()
		again, err := e.subs.OpenCheckout(ctx, c.Subscription.ID, tokenOf(c))
		if err != nil || again.Action.URL != first.Action.URL {
			t.Fatalf("after the hosts are back: %+v %v", again, err)
		}
	})
}
