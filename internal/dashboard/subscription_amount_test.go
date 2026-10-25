package dashboard

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

// planConn is a provider-managed connector that keeps the plan of card
// subscriptions (Razorpay does) and records the amounts it was asked for.
type planConn struct {
	connector.Connector
	mu      *sync.Mutex
	asked   *[]int64
	refusal error
}

func (c planConn) Info() connector.Info {
	return connector.Info{Name: "razorpay", Version: "1", DisplayName: "Razorpay", Currencies: []string{"INR"},
		SubscriptionModes: []connector.BillingMode{connector.ProviderManaged}, SubscriptionFixedMethods: []string{"card"}}
}
func (c planConn) CreateSubscriptionCheckout(context.Context, connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{}, connector.ErrUnsupported
}
func (c planConn) FetchSubscription(context.Context, connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	return connector.SubscriptionSnapshot{}, connector.ErrNotFound
}
func (c planConn) CancelSubscription(context.Context, connector.SubscriptionRef, bool) error {
	return nil
}
func (c planConn) UpdateSubscriptionAmount(_ context.Context, _ connector.SubscriptionRef, amount int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.asked = append(*c.asked, amount)
	return c.refusal
}

const cardText = "the provider cannot change the plan of a card subscription; cancel it and start a new one"

// seedManagedSub makes a running provider-managed subscription whose last
// payment used the method.
func (e *env) seedManagedSub(id, method string) store.Subscription {
	e.t.Helper()
	a := e.seedApp("shop-" + strings.ReplaceAll(id, "_", "-"))
	e.seedCustomer("cus_"+id, a.ID, "live", id+"@example.com")
	sub := e.seedSub(id, a.ID, "cus_"+id, "live", "active", func(s *store.Subscription) {
		s.BillingMode, s.ProviderSubscriptionID = "provider_managed", "psub_"+id
	})
	p := e.seedPayment("pay_"+id, a.ID, "live", "succeeded", "ord", 49900, e.clk.Now())
	e.seedAttempt(store.Attempt{ID: "att_" + id, Mode: "live", PaymentID: p.ID, ProviderAccountID: "pa_x", ProviderRef: "pr_" + id,
		Status: "captured", Method: method, AmountReceived: 49900, CurrencyReceived: "INR"})
	now := e.clk.Now()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertCycle(ctx, tx, store.Cycle{ID: "cyc_" + id, SubscriptionID: id, PeriodIndex: 1, PaymentID: p.ID, AttemptID: "att_" + id,
			Amount: 49900, State: "succeeded", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		e.t.Fatal(err)
	}
	return sub
}

func (e *env) enableManaged(refusal error) *[]int64 {
	e.t.Helper()
	var mu sync.Mutex
	asked := &[]int64{}
	subs := subscriptions.New(subscriptions.Options{DB: e.db, Core: e.core, Settings: e.set, Jobs: e.q, BaseURL: "https://pay.example.com", Now: e.clk.Now,
		Connectors: subscriptions.ConnectorsFunc(func(context.Context, string) (connector.Connector, error) {
			return planConn{mu: &mu, asked: asked, refusal: refusal}, nil
		})})
	e.d.o.Subscriptions = subs
	return asked
}

// The page offers a change of amount only where the provider can do it, and
// says why not where it cannot.
func TestSubscriptionPageHidesTheAmountChangeWhereTheProviderKeepsThePlan(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		asked := e.enableManaged(nil)
		e.seedManagedSub("sub_card", "card")
		e.seedManagedSub("sub_nb", "netbanking")
		c := e.roleClient("operator")
		e.setMode(c, "live")

		card := e.pageOK(c, "/dashboard/subscriptions/sub_card")
		has(t, card, cardText)
		lacks(t, card, `name="amount"`, "/dashboard/subscriptions/sub_card/amount")
		nb := e.pageOK(c, "/dashboard/subscriptions/sub_nb")
		has(t, nb, `name="amount"`, "/dashboard/subscriptions/sub_nb/amount")
		lacks(t, nb, cardText)

		// Posting to the card subscription anyway gets the same words, not a generic error.
		tok := csrfOf(t, card)
		r, body := e.post(c, "/dashboard/subscriptions/sub_card/amount", url.Values{"amount": {"599.00"}, "csrf": {tok}})
		if r.StatusCode != 422 || !strings.Contains(body, cardText) {
			t.Errorf("card: %d\n%s", r.StatusCode, body)
		}
		if len(*asked) != 0 {
			t.Error("the provider was asked to change a plan it cannot change")
		}

		// Where it can: the change is made at the provider, recorded and audited.
		tok = csrfOf(t, nb)
		if r, body := e.post(c, "/dashboard/subscriptions/sub_nb/amount", url.Values{"amount": {"599.00"}, "csrf": {tok}}); r.StatusCode != 303 {
			t.Fatalf("change: %d\n%s", r.StatusCode, body)
		}
		if len(*asked) != 1 || (*asked)[0] != 59900 {
			t.Errorf("the provider was asked for %v", *asked)
		}
		if got := e.subOf("sub_nb"); got.PendingAmount != 59900 {
			t.Errorf("pending amount %d", got.PendingAmount)
		}
		if !strings.Contains(strings.Join(e.auditActions(), " "), "subscription.update") {
			t.Errorf("audit: %v", e.auditActions())
		}
		// Bad input and missing rights.
		for _, bad := range []string{"", "0", "-5", "abc", "1e9999"} {
			if r, _ := e.post(c, "/dashboard/subscriptions/sub_nb/amount", url.Values{"amount": {bad}, "csrf": {tok}}); r.StatusCode != 400 {
				t.Errorf("amount %q: %d", bad, r.StatusCode)
			}
		}
		v := e.roleClient("viewer")
		e.setMode(v, "live")
		if r, _ := e.post(v, "/dashboard/subscriptions/sub_nb/amount", url.Values{"amount": {"700"}, "csrf": {csrfOf(t, e.pageOK(v, "/dashboard/subscriptions/sub_nb"))}}); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		lacks(t, e.pageOK(v, "/dashboard/subscriptions/sub_nb"), `name="amount"`)
	})
}

// A provider's refusal is shown on the page, in the provider's words and
// without secrets, not as "internal error".
func TestAProviderRefusalOfTheChangeIsShownOnThePage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.enableManaged(fmt.Errorf("razorpay: %w: the plan cannot be edited (api key rzp_live_ABCDEFGHIJKLMN)", connector.ErrRejected))
		e.seedManagedSub("sub_nb", "netbanking")
		c := e.roleClient("operator")
		e.setMode(c, "live")
		tok := csrfOf(t, e.pageOK(c, "/dashboard/subscriptions/sub_nb"))
		r, body := e.post(c, "/dashboard/subscriptions/sub_nb/amount", url.Values{"amount": {"599"}, "csrf": {tok}})
		if r.StatusCode != 422 || !strings.Contains(body, "the plan cannot be edited") || strings.Contains(body, "rzp_live_ABCDEFGHIJKLMN") {
			t.Errorf("%d\n%s", r.StatusCode, body)
		}
		if got := e.subOf("sub_nb"); got.PendingAmount != 0 {
			t.Errorf("the amount was recorded although the provider refused: %d", got.PendingAmount)
		}
	})
}
