package razorpay

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

func newSubConn(t *testing.T, f *fake) (connector.SubscriptionManager, connector.Deps) {
	t.Helper()
	srv := startFake(t, f)
	d := connectortest.NewDeps(srv, time.Unix(1760000100, 0))
	c, err := New(testConfig, d)
	if err != nil {
		t.Fatal(err)
	}
	return c.(connector.SubscriptionManager), d
}

func subReq() connector.SubscriptionRequest {
	return connector.SubscriptionRequest{
		SubscriptionID: "sub_01", ProviderRef: "sref_01", Amount: 49900, Currency: "INR", Interval: "month", IntervalCount: 1,
		Description: "Pro plan", Customer: connector.Customer{Name: "A B", Email: "a@example.com", Phone: "+919999999999"},
	}
}

func TestCreateSubscriptionCheckoutCachesPlans(t *testing.T) {
	f := newFake("")
	m, d := newSubConn(t, f)
	res, err := m.CreateSubscriptionCheckout(context.Background(), subReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderOrderID != "sub_Nx5" || res.NextAction.Kind != connector.ActionSDK || res.NextAction.ScriptURL != checkoutScript {
		t.Fatalf("res = %+v", res)
	}
	o := res.NextAction.Options
	if o["subscription_id"] != "sub_Nx5" || o["key"] != testKeyID || o["callback_url"] != "https://pay.example.com/return/acc/sref_01" || o["redirect"] != true {
		t.Errorf("options = %+v", o)
	}
	if _, has := o["order_id"]; has {
		t.Error("subscription checkout must not carry order_id")
	}
	var plan struct {
		Period   string `json:"period"`
		Interval int    `json:"interval"`
		Item     struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"item"`
	}
	_ = json.Unmarshal(f.callsTo("POST", "/v1/plans")[0].Body, &plan)
	if plan.Period != "monthly" || plan.Interval != 1 || plan.Item.Amount != 49900 || plan.Item.Currency != "INR" {
		t.Errorf("plan body = %+v", plan)
	}
	var sub struct {
		PlanID        string            `json:"plan_id"`
		TotalCount    int               `json:"total_count"`
		StartAt       int64             `json:"start_at"`
		CustomerNotif int               `json:"customer_notify"`
		Notes         map[string]string `json:"notes"`
	}
	_ = json.Unmarshal(f.callsTo("POST", "/v1/subscriptions")[0].Body, &sub)
	if sub.PlanID != "plan_Nx1" || sub.TotalCount <= 0 || sub.StartAt != 0 || sub.CustomerNotif != 1 || sub.Notes["sub"] != "sub_01" || sub.Notes["ref"] != "sref_01" {
		t.Errorf("sub body = %+v", sub)
	}
	// Same amount/currency/interval reuses the cached plan.
	if _, err := m.CreateSubscriptionCheckout(context.Background(), subReq()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo("POST", "/v1/plans")); n != 1 {
		t.Errorf("plans created = %d, want 1 (cached)", n)
	}
	if b, ok, _ := d.KV.Get(context.Background(), "razorpay:plan:INR:49900:monthly:1"); !ok || string(b) != "plan_Nx1" {
		t.Errorf("kv plan = %q %v", b, ok)
	}
	// A different amount creates a new plan.
	r := subReq()
	r.Amount = 99900
	if _, err := m.CreateSubscriptionCheckout(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo("POST", "/v1/plans")); n != 2 {
		t.Errorf("plans created = %d, want 2", n)
	}
}

func TestCreateSubscriptionStartAtAndValidation(t *testing.T) {
	f := newFake("")
	m, _ := newSubConn(t, f)
	r := subReq()
	r.StartAt = 1762000000000 // ms
	r.TotalCount = 12
	r.Interval, r.IntervalCount = "year", 2
	if _, err := m.CreateSubscriptionCheckout(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var sub struct {
		StartAt    int64 `json:"start_at"`
		TotalCount int   `json:"total_count"`
	}
	_ = json.Unmarshal(f.callsTo("POST", "/v1/subscriptions")[0].Body, &sub)
	if sub.StartAt != 1762000000 || sub.TotalCount != 12 {
		t.Errorf("sub = %+v", sub)
	}
	var plan struct {
		Period   string `json:"period"`
		Interval int    `json:"interval"`
	}
	_ = json.Unmarshal(f.callsTo("POST", "/v1/plans")[0].Body, &plan)
	if plan.Period != "yearly" || plan.Interval != 2 {
		t.Errorf("plan = %+v", plan)
	}
	for _, bad := range []func(*connector.SubscriptionRequest){
		func(r *connector.SubscriptionRequest) { r.Interval = "fortnight" },
		func(r *connector.SubscriptionRequest) { r.Amount = 0 },
		func(r *connector.SubscriptionRequest) { r.ProviderRef = "" },
		func(r *connector.SubscriptionRequest) { r.IntervalCount = 0 },
	} {
		rr := subReq()
		bad(&rr)
		if _, err := m.CreateSubscriptionCheckout(context.Background(), rr); err == nil {
			t.Errorf("accepted %+v", rr)
		}
	}
}

func TestFetchSubscription(t *testing.T) {
	f := newFake("")
	f.subStatus = "active"
	m, _ := newSubConn(t, f)
	s, err := m.FetchSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "sub_Nx5"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "active" || s.RawStatus != "active" || s.Amount != 49900 || s.Currency != "INR" ||
		s.CurrentPeriodStart != 1760000000000 || s.CurrentPeriodEnd != 1762592000000 {
		t.Errorf("s = %+v", s)
	}
	if _, err := m.FetchSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "missing"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestSubscriptionStatusMapTotal(t *testing.T) {
	for raw, want := range map[string]string{
		"created": "incomplete", "authenticated": "incomplete", "active": "active", "pending": "past_due", "halted": "past_due",
		"paused": "paused", "cancelled": "canceled", "completed": "ended", "expired": "incomplete_expired", "brand_new": "active",
	} {
		if got := mapSubscriptionStatus(raw); got != want {
			t.Errorf("%s -> %s want %s", raw, got, want)
		}
	}
}

func TestCancelSubscription(t *testing.T) {
	f := newFake("")
	m, _ := newSubConn(t, f)
	ref := connector.SubscriptionRef{ProviderSubscriptionID: "sub_Nx5"}
	for _, atEnd := range []bool{true, false} {
		if err := m.CancelSubscription(context.Background(), ref, atEnd); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.callsTo("POST", "/v1/subscriptions/sub_Nx5/cancel")
	var b0, b1 struct {
		AtEnd int `json:"cancel_at_cycle_end"`
	}
	_ = json.Unmarshal(calls[0].Body, &b0)
	_ = json.Unmarshal(calls[1].Body, &b1)
	if len(calls) != 2 || b0.AtEnd != 1 || b1.AtEnd != 0 {
		t.Errorf("cancel bodies = %s / %s", calls[0].Body, calls[1].Body)
	}
}

func TestUpdateSubscriptionAmountSwitchesPlanAtCycleEnd(t *testing.T) {
	f := newFake("")
	f.subStatus = "active"
	m, _ := newSubConn(t, f)
	ref := connector.SubscriptionRef{ProviderSubscriptionID: "sub_Nx5"}
	if err := m.UpdateSubscriptionAmount(context.Background(), ref, 79900); err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Period   string `json:"period"`
		Interval int    `json:"interval"`
		Item     struct {
			Amount int64 `json:"amount"`
		} `json:"item"`
	}
	_ = json.Unmarshal(f.callsTo("POST", "/v1/plans")[0].Body, &plan)
	if plan.Item.Amount != 79900 || plan.Period != "monthly" || plan.Interval != 1 { // period copied from the current plan
		t.Errorf("new plan = %+v", plan)
	}
	var patch struct {
		PlanID   string `json:"plan_id"`
		Schedule string `json:"schedule_change_at"`
	}
	_ = json.Unmarshal(f.callsTo("PATCH", "/v1/subscriptions/sub_Nx5")[0].Body, &patch)
	if patch.PlanID != "plan_Nx1" || patch.Schedule != "cycle_end" {
		t.Errorf("patch = %+v", patch)
	}
	if err := m.UpdateSubscriptionAmount(context.Background(), ref, 0); err == nil {
		t.Error("zero amount accepted")
	}
}

func TestParseSubscriptionWebhooks(t *testing.T) {
	f := newFake("")
	srv := startFake(t, f)
	d := connectortest.NewDeps(srv, time.Unix(1760000100, 0))
	// Plan metadata known from an earlier checkout fills the amount.
	_ = d.KV.Set(context.Background(), "razorpay:planinfo:plan_Nx4", []byte(`{"amount":49900,"currency":"INR","period":"monthly","interval":1}`), 0)
	c, _ := New(testConfig, d)
	cases := []struct {
		file, event, want string
	}{
		{"event_subscription_activated", "subscription.activated", "active"},
		{"event_subscription_charged", "subscription.charged", "active"},
		{"event_subscription_halted", "subscription.halted", "past_due"},
		{"event_subscription_cancelled", "subscription.cancelled", "canceled"},
		{"event_subscription_pending", "subscription.pending", "past_due"},
		{"event_subscription_completed", "subscription.completed", "ended"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			ns, err := c.ParseWebhook(context.Background(), webhookReq(t, tc.file, testWebhookSecret, "evt_s"))
			if err != nil || len(ns) == 0 {
				t.Fatalf("ns=%v err=%v", ns, err)
			}
			n := ns[0]
			if n.Kind != connector.NotifySubscription || n.EventType != tc.event || n.EventID != "evt_s" {
				t.Fatalf("n = %+v", n)
			}
			s := n.Subscription
			if s.ProviderSubscriptionID != "sub_Nx5" || s.Status != tc.want || s.Amount != 49900 || s.Currency != "INR" ||
				s.CurrentPeriodEnd != 1762592000000 {
				t.Errorf("sub = %+v", s)
			}
			if tc.event == "subscription.charged" {
				if len(ns) != 2 || ns[1].Kind != connector.NotifyPayment || ns[1].Payment.Status != connector.PaymentSucceeded || ns[1].EventID != "evt_s" {
					t.Errorf("charged must also carry the renewal payment: %+v", ns)
				}
			} else if len(ns) != 1 {
				t.Errorf("len = %d", len(ns))
			}
		})
	}
}

func TestSubscriptionReturnSignature(t *testing.T) {
	c := newConn(t, newFake("")) // subscription returns sign payment_id|subscription_id
	form := func(sig string) map[string][]string {
		return map[string][]string{"razorpay_payment_id": {"pay_Nx1"}, "razorpay_subscription_id": {"sub_Nx5"}, "razorpay_signature": {sig}}
	}
	ok, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "sref_01", Form: form(sign(testKeySecret, []byte("pay_Nx1|sub_Nx5")))})
	if err != nil || ok.ProviderOrderID != "sub_Nx5" || ok.ProviderPaymentID != "pay_Nx1" {
		t.Fatalf("ok=%+v err=%v", ok, err)
	}
	// The order-style message (order|payment) must not verify a subscription return.
	if _, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "sref_01", Form: form(sign(testKeySecret, []byte("sub_Nx5|pay_Nx1")))}); !errors.Is(err, connector.ErrInvalidReturn) {
		t.Errorf("err = %v", err)
	}
}

func TestInfoDeclaresProviderManaged(t *testing.T) {
	i := newConn(t, newFake("")).Info()
	if !i.Has(connector.CapSubscriptionManager) || len(i.SubscriptionModes) != 1 || i.SubscriptionModes[0] != connector.ProviderManaged {
		t.Errorf("info = %+v", i)
	}
}
