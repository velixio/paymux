package httpapi_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/httpapi"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

// mandateFake is a connector that can only do UPI/card mandates.
type mandateFake struct{}

func (mandateFake) Info() connector.Info {
	return connector.Info{Name: "payu", Version: "1", DisplayName: "PayU", Currencies: []string{"INR"},
		SubscriptionModes: []connector.BillingMode{connector.MerchantTriggered},
		MandateLimits:     []connector.MandateLimit{{Method: "upi", Currency: "INR"}, {Method: "card", Currency: "INR"}}}
}
func (mandateFake) CreateCheckout(context.Context, connector.CheckoutRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{}, connector.ErrUnsupported
}
func (mandateFake) HandleReturn(context.Context, connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{}, connector.ErrUnsupported
}
func (mandateFake) VerifyWebhook(context.Context, connector.WebhookRequest) error { return nil }
func (mandateFake) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	return nil, nil
}
func (mandateFake) FetchPayment(context.Context, connector.PaymentRef) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{}, connector.ErrNotFound
}
func (mandateFake) CreateMandateCheckout(context.Context, connector.MandateRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{}, connector.ErrUnsupported
}
func (mandateFake) FetchMandate(context.Context, connector.MandateRef) (connector.MandateSnapshot, error) {
	return connector.MandateSnapshot{}, connector.ErrNotFound
}
func (mandateFake) NotifyPreDebit(context.Context, connector.PreDebitRequest) error { return nil }
func (mandateFake) ChargeMandate(context.Context, connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{}, connector.ErrUnsupported
}
func (mandateFake) RevokeMandate(context.Context, connector.MandateRef) error { return nil }

func newSubFixture(t *testing.T, db *store.DB) *fixture {
	t.Helper()
	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	q := jobs.New(db, jobs.Options{})
	svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: q})
	subs := subscriptions.New(subscriptions.Options{DB: db, Core: svc, Settings: set, Jobs: q, BaseURL: "https://pay.example.com",
		Connectors: subscriptions.ConnectorsFunc(func(context.Context, string) (connector.Connector, error) { return mandateFake{}, nil })})
	f := &fixture{t: t, db: db, svc: svc}
	var err error
	if f.app, err = svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "One", Slug: "one"}); err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "Two", Slug: "two"})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_live", OrgID: store.DefaultOrgID, Mode: "live",
			Connector: "payu", Name: "PayU", Status: "active", AcceptedCurrencies: []string{"INR"},
			EnabledPurposes: []string{"one_time", "subscription"}, CreatedAt: time.Now()}); err != nil {
			return err
		}
		return store.LinkAppProviderAccount(ctx, tx, f.app.ID, "pa_live")
	})
	if err != nil {
		t.Fatal(err)
	}
	f.liveKey, _, _ = svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, f.app.ID, "live")
	f.testKey, _, _ = svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, f.app.ID, "test")
	f.otherKey, _, _ = svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, other.ID, "live")
	box, _ := secrets.NewBox(bytes.Repeat([]byte{7}, 32))
	base, _ := url.Parse("https://pay.example.com")
	api, err := httpapi.New(httpapi.Options{Core: svc, DB: db, Box: box, BaseURL: base, Subscriptions: subs})
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Options{})
	api.Register(s)
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

var subBody = map[string]any{"customer": map[string]any{"external_ref": "u1", "email": "a@example.com", "name": "Asha"},
	"amount": 49900, "currency": "INR", "interval": "month", "description": "Pro", "method": "upi",
	"metadata": map[string]string{"plan": "pro"}}

func TestSubscriptionAPI(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newSubFixture(t, db)
		r := f.do("POST", "/v1/subscriptions", f.liveKey, subBody, "Idempotency-Key", "k1")
		if r.status != 201 {
			t.Fatalf("create: %d %s", r.status, r.raw)
		}
		sub := r.json(t)
		id := sub["id"].(string)
		if sub["object"] != "subscription" || sub["status"] != "incomplete" || sub["billing_mode"] != "merchant_triggered" ||
			sub["amount"] != float64(49900) || sub["interval_count"] != float64(1) || sub["requires_reauthorization"] != false ||
			!strings.HasPrefix(sub["checkout_url"].(string), "https://pay.example.com/s/"+id+"?t=") || sub["metadata"].(map[string]any)["plan"] != "pro" {
			t.Fatalf("subscription = %v", sub)
		}
		if strings.Contains(string(r.raw), "token_hash") {
			t.Error("token hash leaked")
		}
		// Idempotent replay.
		if again := f.do("POST", "/v1/subscriptions", f.liveKey, subBody, "Idempotency-Key", "k1"); again.status != 201 || again.header.Get("Idempotent-Replay") != "true" || again.json(t)["id"] != id {
			t.Errorf("replay: %d %s", again.status, again.raw)
		}

		// Get and list; no checkout_url after creation.
		g := f.do("GET", "/v1/subscriptions/"+id, f.liveKey, nil)
		if g.status != 200 || g.json(t)["id"] != id || g.json(t)["checkout_url"] != nil {
			t.Errorf("get: %d %s", g.status, g.raw)
		}
		l := f.do("GET", "/v1/subscriptions?status=incomplete&limit=5", f.liveKey, nil)
		if l.status != 200 || len(l.json(t)["data"].([]any)) != 1 {
			t.Errorf("list: %d %s", l.status, l.raw)
		}
		if l := f.do("GET", "/v1/subscriptions?status=active", f.liveKey, nil); len(l.json(t)["data"].([]any)) != 0 {
			t.Errorf("status filter: %s", l.raw)
		}
		if l := f.do("GET", "/v1/subscriptions?limit=0", f.liveKey, nil); l.status != 400 {
			t.Errorf("bad limit: %d", l.status)
		}

		// Scoping.
		for _, key := range []string{f.otherKey, f.testKey} {
			if g := f.do("GET", "/v1/subscriptions/"+id, key, nil); g.status != 404 || g.errType(t) != "not_found" {
				t.Errorf("foreign read: %d %s", g.status, g.raw)
			}
			if c := f.do("POST", "/v1/subscriptions/"+id+"/cancel", key, nil); c.status != 404 {
				t.Errorf("foreign cancel: %d", c.status)
			}
		}
		if g := f.do("GET", "/v1/subscriptions/"+id, "", nil); g.status != 401 {
			t.Errorf("unauthenticated: %d", g.status)
		}

		// Errors keep their own types.
		cases := []struct {
			name   string
			body   map[string]any
			status int
			typ    string
		}{
			{"missing amount", map[string]any{"customer": subBody["customer"], "currency": "INR", "interval": "month"}, 400, "invalid_request"},
			{"bad interval", mut(subBody, "interval", "fortnight"), 400, "invalid_request"},
			{"over the mandate limit", mut(subBody, "amount", 2_000_000), 422, "amount_exceeds_mandate_limit"},
			{"no route for the currency", mut(subBody, "currency", "USD"), 422, "no_route"},
		}
		for _, c := range cases {
			if r := f.do("POST", "/v1/subscriptions", f.liveKey, c.body); r.status != c.status || r.errType(t) != c.typ {
				t.Errorf("%s: %d %s", c.name, r.status, r.raw)
			}
		}

		// Update and payment method need an active subscription.
		if u := f.do("POST", "/v1/subscriptions/"+id+"/update", f.liveKey, map[string]any{"amount": 60000}); u.status != 409 || u.errType(t) != "invalid_state" {
			t.Errorf("update while incomplete: %d %s", u.status, u.raw)
		}
		if u := f.do("POST", "/v1/subscriptions/"+id+"/update", f.liveKey, map[string]any{}); u.status != 400 {
			t.Errorf("update without amount: %d", u.status)
		}
		if u := f.do("POST", "/v1/subscriptions/"+id+"/payment_method", f.liveKey, nil); u.status != 409 {
			t.Errorf("payment_method while incomplete: %d", u.status)
		}

		// Cancel.
		c := f.do("POST", "/v1/subscriptions/"+id+"/cancel", f.liveKey, map[string]any{"at_period_end": false})
		if c.status != 200 || c.json(t)["status"] != "canceled" || c.json(t)["canceled_at"] == nil {
			t.Fatalf("cancel: %d %s", c.status, c.raw)
		}
		if c := f.do("POST", "/v1/subscriptions/"+id+"/cancel", f.liveKey, nil); c.status != 200 {
			t.Errorf("second cancel: %d", c.status)
		}
		if u := f.do("POST", "/v1/subscriptions/"+id+"/cancel", f.liveKey, map[string]any{"bogus": 1}); u.status != 400 {
			t.Errorf("unknown field: %d", u.status)
		}
	})
}

func mut(m map[string]any, k string, v any) map[string]any {
	out := map[string]any{}
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

func TestSubscriptionRoutesWithoutServiceAre404(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		if g := f.do("GET", "/v1/subscriptions/sub_x", f.testKey, nil); g.status != 404 {
			t.Errorf("status = %d", g.status)
		}
	})
}
