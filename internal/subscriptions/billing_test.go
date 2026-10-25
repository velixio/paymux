package subscriptions_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

// A subscription whose app sent no billing details used to be taxed as if the
// buyer lived in the supplier's state. The hosted page now asks, like the pay
// page does, before anything is started at the provider.

var csrfRE = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

var noBilling = &core.CustomerInput{ExternalRef: "u2", Name: "Ravi", Email: "ravi@example.com"}

const gstin29 = "29AAAAA0000A1Z5"

type page struct {
	status int
	loc    string
	body   string
}

// billingServer mounts the hosted subscription routes.
func billingServer(e *env) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /s/{id}", e.subs.ServeHosted)
	mux.HandleFunc("POST /s/{id}", e.subs.SubmitBilling)
	srv := httptest.NewServer(mux)
	e.t.Cleanup(srv.Close)
	return srv
}

func doPage(t *testing.T, method, target string, form url.Values) page {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return page{res.StatusCode, res.Header.Get("Location"), string(b)}
}

// answer loads the billing form of the checkout at target and submits it with
// the given fields (and the form's own CSRF token).
func answer(t *testing.T, target string, fields url.Values) page {
	t.Helper()
	p := doPage(t, "GET", target, nil)
	m := csrfRE.FindStringSubmatch(p.body)
	if m == nil {
		t.Fatalf("no billing form with a CSRF token (%d):\n%s", p.status, p.body)
	}
	f := url.Values{"csrf": {m[1]}}
	for k, v := range fields {
		f[k] = v
	}
	return doPage(t, "POST", target, f)
}

func TestHostedCheckoutAsksForBillingBeforeTheProviderIsCalled(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := billingServer(e)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Customer = noBilling })
		u, _ := url.Parse(c.CheckoutURL)
		target := srv.URL + u.RequestURI()

		p := doPage(t, "GET", target, nil)
		if p.status != 200 || !strings.Contains(p.body, `name="state_code"`) || !strings.Contains(p.body, `name="country"`) || !strings.Contains(p.body, "Karnataka") {
			t.Fatalf("billing form: %d\n%s", p.status, p.body)
		}
		if len(e.r.mandateReqs) != 0 {
			t.Fatal("the provider checkout was started before the buyer gave billing details")
		}
		// Nothing valid, nothing stored, still no provider call.
		for name, fields := range map[string]url.Values{
			"no state":    {"name": {"Ravi"}},
			"bad state":   {"name": {"Ravi"}, "state_code": {"99"}},
			"bad gstin":   {"name": {"Ravi"}, "gstin": {"29AAAAA0000A1Z6"}, "legal_name": {"X"}, "state_code": {"29"}},
			"bad country": {"name": {"Ravi"}, "country": {"USA"}},
		} {
			if r := answer(t, target, fields); r.status != 400 || !strings.Contains(r.body, `name="state_code"`) {
				t.Errorf("%s: %d", name, r.status)
			}
		}
		if len(e.r.mandateReqs) != 0 {
			t.Fatal("the provider was called for an invalid form")
		}
		// A form without the CSRF token of this checkout is refused.
		if r := doPage(t, "POST", target, url.Values{"csrf": {"deadbeef"}, "name": {"Ravi"}, "state_code": {"29"}}); r.status != 403 {
			t.Errorf("wrong CSRF token: %d", r.status)
		}

		r := answer(t, target, url.Values{"name": {"Ravi"}, "state_code": {"29"}})
		if r.status != 303 || !strings.HasPrefix(r.loc, "/s/"+c.Subscription.ID+"?t=") {
			t.Fatalf("submit: %d %s", r.status, r.loc)
		}
		cust, err := e.core.GetCustomer(ctx, e.sub(c.Subscription.ID).CustomerID)
		if err != nil || cust.Billing.StateCode != "29" || cust.Billing.Country != "IN" {
			t.Errorf("customer = %+v %v", cust.Billing, err)
		}
		// Now the buyer goes on to the provider.
		p = doPage(t, "GET", target, nil)
		if p.status != 200 || !strings.Contains(p.body, `action="https://pg.example.com/si"`) || len(e.r.mandateReqs) != 1 {
			t.Errorf("after the form: %d requests=%d\n%s", p.status, len(e.r.mandateReqs), p.body)
		}
	})
}

func TestHostedCheckoutSkipsTheFormWhenTheApiSentBillingDetails(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := billingServer(e)
		c := e.mustCreate(nil) // the default customer has a state and a country
		u, _ := url.Parse(c.CheckoutURL)
		if p := doPage(t, "GET", srv.URL+u.RequestURI(), nil); p.status != 200 || strings.Contains(p.body, `name="state_code"`) || !strings.Contains(p.body, "pg.example.com") {
			t.Errorf("%d %s", p.status, p.body)
		}
	})
}

// A buyer abroad is asked for the country even when the subscription is in
// dollars, where no Indian State applies.
func TestHostedCheckoutForADollarSubscriptionAsksForTheCountry(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := billingServer(e)
		e.routeTo("whop")
		c := e.mustCreate(func(i *subscriptions.CreateInput) {
			i.Method = ""
			i.Customer = noBilling
			i.Currency = "USD"
			i.Amount = 1500
		})
		u, _ := url.Parse(c.CheckoutURL)
		target := srv.URL + u.RequestURI()
		p := doPage(t, "GET", target, nil)
		if p.status != 200 || !strings.Contains(p.body, `name="country"`) || strings.Contains(p.body, `name="state_code"`) {
			t.Fatalf("form: %d\n%s", p.status, p.body)
		}
		if r := answer(t, target, url.Values{"name": {"Ravi"}}); r.status != 400 {
			t.Errorf("no country: %d", r.status)
		}
		if r := answer(t, target, url.Values{"name": {"Ravi"}, "country": {"de"}}); r.status != 303 {
			t.Fatalf("with a country: %d %s", r.status, r.body)
		}
		cust, _ := e.core.GetCustomer(ctx, e.sub(c.Subscription.ID).CustomerID)
		if cust.Billing.Country != "DE" || cust.Billing.StateCode != "" {
			t.Errorf("customer = %+v", cust.Billing)
		}
	})
}

func TestHostedCheckoutFollowsTheBillingDetailsSetting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, t0)
		srv := billingServer(e)
		e.setSetting("billing_details", `"off"`)
		c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Customer = noBilling })
		u, _ := url.Parse(c.CheckoutURL)
		if p := doPage(t, "GET", srv.URL+u.RequestURI(), nil); strings.Contains(p.body, `name="state_code"`) || !strings.Contains(p.body, "pg.example.com") {
			t.Errorf("setting off: %d %s", p.status, p.body)
		}
	})
}

// The renewal invoices of the subscription use what the buyer gave on the page.
func TestRenewalInvoicesUseTheBillingDetailsGivenOnTheHostedPage(t *testing.T) {
	for name, tc := range map[string]struct {
		fields    url.Values
		wantType  string
		wantPlace string
		igst      bool
		cgst      bool
	}{
		"in the supplier's state": {url.Values{"name": {"Ravi"}, "state_code": {"27"}}, "b2c", "27", false, true},
		"another state":           {url.Values{"name": {"Ravi"}, "state_code": {"29"}}, "b2c", "29", true, false},
		"a business (GSTIN)":      {url.Values{"name": {"Ravi"}, "gstin": {gstin29}, "legal_name": {"Acme Pvt Ltd"}}, "b2b", "29", true, false},
		"a buyer abroad":          {url.Values{"name": {"Ravi"}, "country": {"US"}}, "export_igst", "OUTSIDE_INDIA", true, false},
	} {
		t.Run(name, func(t *testing.T) {
			storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
				e := newEnv(t, db, t0)
				srv := billingServer(e)
				e.routeTo("razorpay")
				c := e.mustCreate(func(i *subscriptions.CreateInput) { i.Method = ""; i.Customer = noBilling })
				u, _ := url.Parse(c.CheckoutURL)
				target := srv.URL + u.RequestURI()
				if r := answer(t, target, tc.fields); r.status != 303 {
					t.Fatalf("form: %d %s", r.status, r.body)
				}
				if p := doPage(t, "GET", target, nil); p.status != 200 || len(e.r.subReqs) != 1 {
					t.Fatalf("checkout: %d requests=%d", p.status, len(e.r.subReqs))
				}
				if err := db.Write(ctx, func(tx store.Tx) error {
					_, err := tx.ExecContext(ctx, "UPDATE subscriptions SET provider_subscription_id = 'sub_Nx5' WHERE id = ?", c.Subscription.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for _, fixture := range []string{"event_subscription_activated", "event_subscription_charged"} {
					for _, n := range rzp(t, fixture) {
						if n.Subscription != nil {
							e.r.mu.Lock()
							if e.r.subTruth == nil {
								e.r.subTruth = map[string]connector.SubscriptionSnapshot{}
							}
							e.r.subTruth[n.Subscription.ProviderSubscriptionID] = *n.Subscription
							e.r.mu.Unlock()
						}
						if err := e.notify("pa_managed", n); err != nil {
							t.Fatalf("%s: %v", fixture, err)
						}
					}
				}
				pays := e.renewalPayments(c.Subscription.ID)
				if len(pays) != 1 {
					t.Fatalf("payments = %+v", pays)
				}
				var inv store.Invoice
				_ = db.Read(ctx, func(q store.Querier) error {
					invs, _ := store.InvoicesForPayment(ctx, q, pays[0].ID)
					if len(invs) == 1 {
						inv = invs[0]
					}
					return nil
				})
				if inv.ID == "" {
					t.Fatal("no invoice")
				}
				if inv.SupplyType != tc.wantType || inv.PlaceOfSupply != tc.wantPlace || (inv.IGST > 0) != tc.igst || (inv.CGST > 0) != tc.cgst || inv.HasFlag("pos_defaulted") {
					t.Errorf("invoice: type=%s place=%s igst=%d cgst=%d sgst=%d flags=%v", inv.SupplyType, inv.PlaceOfSupply, inv.IGST, inv.CGST, inv.SGST, inv.Flags)
				}
			})
		})
	}
}
