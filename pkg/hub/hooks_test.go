package hub

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
)

func hookApp(t *testing.T) *App {
	t.Helper()
	app, err := New(testConfig(t, "sqlite://"+t.TempDir()+"/x.db", t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func ev(typ, objType, id, data string) hookEvent {
	return hookEvent{Type: typ, OrgID: "org_1", AppID: "app_1", Mode: "live", ObjectType: objType, ObjectID: id, ObjectVersion: 3, Data: []byte(data)}
}

func TestHooksReceiveTypedReadOnlySnapshots(t *testing.T) {
	app := hookApp(t)
	var got []any
	record := func(v any) error { got = append(got, v); return nil }
	app.OnPaymentSucceeded(func(_ context.Context, e PaymentEvent) error { return record(e) })
	app.OnPaymentFailed(func(_ context.Context, e PaymentEvent) error { return record(e) })
	app.OnRefundSucceeded(func(_ context.Context, e RefundEvent) error { return record(e) })
	app.OnDisputeOpened(func(_ context.Context, e DisputeEvent) error { return record(e) })
	app.OnSubscriptionRenewed(func(_ context.Context, e SubscriptionEvent) error { return record(e) })
	app.OnInvoiceIssued(func(_ context.Context, e InvoiceEvent) error { return record(e) })

	for _, typ := range []string{"payment.succeeded", "payment.failed", "refund.succeeded", "dispute.opened", "subscription.renewed", "invoice.issued"} {
		if !app.hooks.wants(typ) {
			t.Errorf("no hook registered for %s", typ)
		}
	}
	if app.hooks.wants("payment.created") || app.hooks.wants("refund.failed") {
		t.Error("events without a hook must not be queued")
	}

	ctx := context.Background()
	app.runHooks(ctx, ev("payment.succeeded", "payment", "pay_1", `{"id":"pay_1","object":"payment","status":"succeeded","kind":"one_time","amount":49900,"currency":"INR",
		"amount_received":49900,"amount_refunded":0,"order_ref":"ord-1","description":"Pro","customer_id":"cus_1","metadata":{"plan":"pro"},
		"expires_at":"2026-10-17T12:00:00Z","created_at":"2026-10-17T11:00:00Z","version":3,"flags":["review"],"attempt_id":"att_1"}`))
	app.runHooks(ctx, ev("payment.failed", "payment", "pay_2", `{"id":"pay_2","status":"failed","amount":100,"currency":"USD","reason":"declined","attempt_id":"att_2"}`))
	app.runHooks(ctx, ev("refund.succeeded", "refund", "rfn_1", `{"id":"rfn_1","payment_id":"pay_1","attempt_id":"att_1","amount":10000,"currency":"INR","status":"succeeded","reason":"customer request","initiated_by":"api"}`))
	app.runHooks(ctx, ev("dispute.opened", "dispute", "dsp_1", `{"id":"dsp_1","payment_id":"pay_1","amount":49900,"currency":"INR","reason":"fraud","status":"needs_response","stage":"chargeback","due_by":"2026-10-30T00:00:00Z","source":"webhook"}`))
	app.runHooks(ctx, ev("subscription.renewed", "subscription", "sub_1", `{"id":"sub_1","status":"active","billing_mode":"merchant_triggered","customer_id":"cus_1","amount":19900,"currency":"INR",
		"interval":"month","interval_count":1,"current_period_start":"2026-10-01T00:00:00Z","current_period_end":"2026-11-01T00:00:00Z","renewals_done":2,"metadata":{"seat":"3"}}`))
	app.runHooks(ctx, ev("invoice.issued", "invoice", "inv_1", `{"id":"inv_1","number":"VS/2627/000001","payment_id":"pay_1","currency":"INR","supply_type":"b2c","place_of_supply":"Karnataka (29)",
		"taxable":42288,"cgst":3806,"sgst":3806,"igst":0,"total":49900,"issued_at":"2026-10-17T11:30:00Z"}`))
	if len(got) != 6 {
		t.Fatalf("hooks ran %d times, want 6", len(got))
	}

	p := got[0].(PaymentEvent)
	if p.Event.Type != "payment.succeeded" || p.Event.AppID != "app_1" || p.Event.Mode != "live" || p.Event.ObjectID != "pay_1" || p.Event.ObjectVersion != 3 {
		t.Errorf("event info %+v", p.Event)
	}
	if p.Payment.ID != "pay_1" || p.Payment.Amount != 49900 || p.Payment.AmountReceived != 49900 || p.Payment.Currency != "INR" || p.Payment.OrderRef != "ord-1" ||
		p.Payment.CustomerID != "cus_1" || p.Payment.Metadata["plan"] != "pro" || !slices.Equal(p.Payment.Flags, []string{"review"}) || p.AttemptID != "att_1" ||
		!p.Payment.CreatedAt.Equal(time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("payment %+v", p)
	}
	if !strings.Contains(string(p.Raw), `"order_ref":"ord-1"`) {
		t.Errorf("raw event object missing: %s", p.Raw)
	}
	if f := got[1].(PaymentEvent); f.Payment.ID != "pay_2" || f.Reason != "declined" || f.Payment.Currency != "USD" {
		t.Errorf("failed payment %+v", f)
	}
	if r := got[2].(RefundEvent); r.Refund.ID != "rfn_1" || r.Refund.PaymentID != "pay_1" || r.Refund.Amount != 10000 || r.Refund.Reason != "customer request" || r.Refund.Status != "succeeded" {
		t.Errorf("refund %+v", r)
	}
	if d := got[3].(DisputeEvent); d.Dispute.ID != "dsp_1" || d.Dispute.Amount != 49900 || d.Dispute.Status != "needs_response" || d.Dispute.Stage != "chargeback" ||
		!d.Dispute.DueBy.Equal(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dispute %+v", d)
	}
	if s := got[4].(SubscriptionEvent); s.Subscription.ID != "sub_1" || s.Subscription.Status != "active" || s.Subscription.RenewalsDone != 2 || s.Subscription.Interval != "month" ||
		s.Subscription.Amount != 19900 || s.Subscription.Metadata["seat"] != "3" {
		t.Errorf("subscription %+v", s)
	}
	if i := got[5].(InvoiceEvent); i.Invoice.Number != "VS/2627/000001" || i.Invoice.Total != 49900 || i.Invoice.CGST != 3806 || i.Invoice.PaymentID != "pay_1" {
		t.Errorf("invoice %+v", i)
	}
}

func TestHookSnapshotsAreIndependentCopies(t *testing.T) {
	app := hookApp(t)
	var second map[string]string
	app.OnPaymentSucceeded(func(_ context.Context, e PaymentEvent) error {
		e.Payment.Metadata["plan"] = "tampered"
		e.Payment.Flags = append(e.Payment.Flags, "x")
		e.Raw[0] = 'X'
		return nil
	})
	app.OnPaymentSucceeded(func(_ context.Context, e PaymentEvent) error { second = e.Payment.Metadata; return nil })
	app.runHooks(context.Background(), ev("payment.succeeded", "payment", "pay_1", `{"id":"pay_1","metadata":{"plan":"pro"}}`))
	if second["plan"] != "pro" {
		t.Errorf("a hook changed what the next hook saw: %v", second)
	}
}

func TestAHookFailureIsReportedAndNeverStopsTheOthers(t *testing.T) {
	app := hookApp(t)
	type failure struct {
		hook string
		ev   hookEvent
		err  error
	}
	var mu sync.Mutex
	var failures []failure
	app.hookFailed = func(_ context.Context, hook string, e hookEvent, err error) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, failure{hook, e, err})
	}
	var ran []string
	app.OnPaymentSucceeded(func(context.Context, PaymentEvent) error {
		ran = append(ran, "error")
		return errors.New("crm is down")
	})
	app.OnPaymentSucceeded(func(context.Context, PaymentEvent) error { ran = append(ran, "panic"); panic("boom") })
	app.OnPaymentSucceeded(func(context.Context, PaymentEvent) error { ran = append(ran, "ok"); return nil })
	app.runHooks(context.Background(), ev("payment.succeeded", "payment", "pay_9", `{"id":"pay_9"}`))

	if !slices.Equal(ran, []string{"error", "panic", "ok"}) {
		t.Errorf("hooks ran %v", ran)
	}
	if len(failures) != 2 {
		t.Fatalf("failures %+v", failures)
	}
	if failures[0].hook != "OnPaymentSucceeded" || failures[0].ev.ObjectID != "pay_9" || failures[0].err.Error() != "crm is down" {
		t.Errorf("first failure %+v", failures[0])
	}
	if !strings.Contains(failures[1].err.Error(), "panic") || !strings.Contains(failures[1].err.Error(), "boom") {
		t.Errorf("a panic is reported as an error: %v", failures[1].err)
	}
}

func TestAHookGetsADeadline(t *testing.T) {
	app := hookApp(t)
	var deadline bool
	app.OnInvoiceIssued(func(ctx context.Context, _ InvoiceEvent) error { _, deadline = ctx.Deadline(); return nil })
	app.runHooks(context.Background(), ev("invoice.issued", "invoice", "inv_1", `{"id":"inv_1"}`))
	if !deadline {
		t.Error("hook context has no deadline, so a stuck hook would hold a job worker forever")
	}
}

func TestOnRouteHooksChainAndCannotBreakRouting(t *testing.T) {
	app := hookApp(t)
	cands := []store.ProviderAccount{{ID: "pa_1", Connector: "razorpay", Name: "main"}, {ID: "pa_2", Connector: "payu", Name: "backup"}}
	req := core.RouteRequest{Scope: core.Scope{AppID: "app_1", Mode: "live"}, Currency: "INR", Country: "IN", Amount: 500, Purpose: "one_time",
		Metadata: map[string]string{"plan": "pro"}}
	if got := app.suggestRoute(context.Background(), req, cands); got != nil {
		t.Errorf("no hooks: %v", got)
	}

	var seen RouteRequest
	app.OnRoute(func(r RouteRequest) []string {
		seen = r
		r.Metadata["plan"] = "tampered" // a copy: must not leak back
		return []string{"pa_2"}
	})
	app.OnRoute(func(r RouteRequest) []string {
		// The second hook sees the order the first one proposed.
		if len(r.Accounts) != 2 || r.Accounts[0].ID != "pa_2" {
			t.Errorf("second hook saw %+v", r.Accounts)
		}
		return nil // no opinion: keep the order
	})
	app.OnRoute(func(RouteRequest) []string { panic("bug in a routing hook") })
	got := app.suggestRoute(context.Background(), req, cands)
	if !slices.Equal(got, []string{"pa_2", "pa_1"}) {
		t.Errorf("suggested order %v", got)
	}
	if seen.AppID != "app_1" || seen.Mode != "live" || seen.Currency != "INR" || seen.Country != "IN" || seen.Amount != 500 || seen.Purpose != "one_time" ||
		len(seen.Accounts) != 2 || seen.Accounts[0] != (RouteAccount{ID: "pa_1", Connector: "razorpay", Name: "main"}) {
		t.Errorf("hook saw %+v", seen)
	}
	if req.Metadata["plan"] != "pro" {
		t.Error("a routing hook changed the request's metadata")
	}
}

func TestHookEventsMarshalAsSnakeCaseJSON(t *testing.T) {
	b, err := json.Marshal(PaymentEvent{Event: EventInfo{Type: "payment.succeeded", OrgID: "org_1", AppID: "app_1", Mode: "live", ObjectType: "payment", ObjectID: "pay_1", ObjectVersion: 3},
		Payment: Payment{ID: "pay_1", Amount: 100, Currency: "INR", AmountReceived: 100, OrderRef: "o1", Metadata: map[string]string{"k": "v"}}, AttemptID: "att_1", Raw: json.RawMessage(`{"id":"pay_1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	ev, _ := m["event"].(map[string]any)
	pay, _ := m["payment"].(map[string]any)
	if ev["type"] != "payment.succeeded" || ev["app_id"] != "app_1" || ev["object_id"] != "pay_1" || ev["object_version"] != float64(3) ||
		pay["amount_received"] != float64(100) || pay["order_ref"] != "o1" || m["attempt_id"] != "att_1" || m["raw"].(map[string]any)["id"] != "pay_1" {
		t.Errorf("shape: %s", b)
	}
	rb, _ := json.Marshal(RouteRequest{AppID: "app_1", Mode: "live", Currency: "INR", Amount: 5, Purpose: "one_time", Accounts: []RouteAccount{{ID: "pa_1", Connector: "razorpay", Name: "main"}}})
	var rm map[string]any
	_ = json.Unmarshal(rb, &rm)
	acc, _ := rm["accounts"].([]any)
	if rm["app_id"] != "app_1" || rm["currency"] != "INR" || len(acc) != 1 || acc[0].(map[string]any)["id"] != "pa_1" || acc[0].(map[string]any)["connector"] != "razorpay" {
		t.Errorf("route request shape: %s", rb)
	}
	for _, e := range []any{RefundEvent{}, DisputeEvent{}, SubscriptionEvent{}, InvoiceEvent{}} {
		out, _ := json.Marshal(e)
		if !strings.Contains(string(out), `"event":{"type"`) {
			t.Errorf("%T lacks a lower-case event object: %s", e, out)
		}
	}
}
