package razorpay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

func newConn(t *testing.T, f *fake) connector.Connector {
	t.Helper()
	srv := startFake(t, f)
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(1760000100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegistered(t *testing.T) {
	f, info, ok := connector.Lookup("razorpay")
	if !ok || f == nil || info.DisplayName == "" {
		t.Fatalf("razorpay not registered: %v %v", info, ok)
	}
}

func TestConfigValidation(t *testing.T) {
	d := connectortest.NewDeps(nil, time.Now())
	for _, cfg := range []string{`{}`, `{"key_id":"a"}`, `{"key_id":"a","key_secret":"b"}`, `not json`} {
		if _, err := New(json.RawMessage(cfg), d); err == nil {
			t.Errorf("config %s accepted", cfg)
		}
	}
}

func TestCreateCheckoutRequestAndOptions(t *testing.T) {
	f := newFake("created")
	c := newConn(t, f)
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{
		PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01",
		Amount: 49900, Currency: "INR", Description: "Pro plan",
		Customer: connector.Customer{Name: "A B", Email: "a@example.com", Phone: "+919999999999"},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.callsTo("POST", "/v1/orders")
	if len(calls) != 1 {
		t.Fatalf("order calls = %d", len(calls))
	}
	var body struct {
		Amount   int64             `json:"amount"`
		Currency string            `json:"currency"`
		Receipt  string            `json:"receipt"`
		Notes    map[string]string `json:"notes"`
	}
	_ = json.Unmarshal(calls[0].Body, &body)
	if body.Amount != 49900 || body.Currency != "INR" || body.Receipt != "pay_ref_01" ||
		body.Notes["pay"] != "pay_01" || body.Notes["att"] != "att_01" || body.Notes["app"] != "app_01" {
		t.Fatalf("order body = %s", calls[0].Body)
	}
	if res.ProviderOrderID != "order_Nx0" {
		t.Errorf("order id = %q", res.ProviderOrderID)
	}
	na := res.NextAction
	if na.Kind != connector.ActionSDK || na.ScriptURL != "https://checkout.razorpay.com/v1/checkout.js" {
		t.Fatalf("next action = %+v", na)
	}
	o := na.Options
	if o["key"] != testKeyID || o["order_id"] != "order_Nx0" || o["amount"] != float64(49900) || o["currency"] != "INR" ||
		o["name"] != "Voice Studio" || o["callback_url"] != "https://pay.example.com/return/acc/pay_ref_01" {
		t.Errorf("options = %+v", o)
	}
	if pf, _ := o["prefill"].(map[string]any); pf["email"] != "a@example.com" || pf["contact"] != "+919999999999" || pf["name"] != "A B" {
		t.Errorf("prefill = %+v", o["prefill"])
	}
	if strings.Contains(string(mustJSON(na)), testKeySecret) {
		t.Error("key secret leaked into NextAction")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestCreateCheckoutReceiptTooLong(t *testing.T) {
	c := newConn(t, newFake("created"))
	_, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{ProviderRef: strings.Repeat("x", 41), Amount: 100, Currency: "INR"})
	if err == nil {
		t.Fatal("41-char receipt accepted")
	}
}

func TestCreateCheckoutBadCredentials(t *testing.T) {
	srv := startFake(t, newFake("created"))
	cfg := strings.Replace(string(testConfig), testKeySecret, "wrong", 1)
	c, _ := New(json.RawMessage(cfg), connectortest.NewDeps(srv, time.Now()))
	_, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{ProviderRef: "r", Amount: 100, Currency: "INR"})
	if !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
}

// An authorized payment is captured by FetchPayment: Razorpay releases the
// money only after capture, otherwise it auto-refunds after ~5 days.
func TestFetchPaymentCapturesAuthorized(t *testing.T) {
	f := newFake("authorized")
	c := newConn(t, f)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "order_Nx0"})
	if err != nil {
		t.Fatal(err)
	}
	caps := f.callsTo("POST", "/v1/payments/pay_Nx1/capture")
	if len(caps) != 1 {
		t.Fatalf("capture calls = %d", len(caps))
	}
	var b struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	_ = json.Unmarshal(caps[0].Body, &b)
	if b.Amount != 49900 || b.Currency != "INR" {
		t.Errorf("capture body = %s", caps[0].Body)
	}
	if snap.Status != connector.PaymentSucceeded || snap.RawStatus != "captured" {
		t.Errorf("snap = %+v", snap)
	}
	// A second fetch sees captured and does not capture again.
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "order_Nx0"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo("POST", "/v1/payments/pay_Nx1/capture")); n != 1 {
		t.Errorf("captured again: %d calls", n)
	}
}

func TestFetchPaymentCaptureAlreadyDone(t *testing.T) {
	f := newFake("authorized")
	f.captureFails = true
	c := newConn(t, f)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_Nx1"})
	if err != nil {
		t.Fatalf("already-captured race should not error: %v", err)
	}
	if snap.Status != connector.PaymentAuthorized && snap.Status != connector.PaymentSucceeded {
		t.Errorf("status = %q", snap.Status)
	}
}

func TestFetchPaymentFeeTaxAndRefs(t *testing.T) {
	c := newConn(t, newFake("captured"))
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "order_Nx0"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Fee != 1178 || snap.Tax != 180 || snap.Method != "upi" || snap.ProviderPaymentID != "pay_Nx1" ||
		snap.ProviderOrderID != "order_Nx0" || snap.Currency != "INR" || snap.CreatedAt != 1760000000000 {
		t.Errorf("snap = %+v", snap)
	}
}

func TestFetchPaymentOrderWithoutPayments(t *testing.T) {
	c := newConn(t, newFake(""))
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "order_Nx0"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.PaymentRequiresAction || snap.RawStatus != "created" || snap.Amount != 49900 {
		t.Errorf("snap = %+v", snap)
	}
}

func TestFetchPaymentByProviderRef(t *testing.T) {
	f := newFake("captured")
	c := newConn(t, f)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentSucceeded {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if got := f.callsTo("GET", "/v1/orders")[0].Query; !strings.Contains(got, "receipt=pay_ref_01") {
		t.Errorf("query = %q", got)
	}
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{}); err == nil {
		t.Error("empty ref accepted")
	}
}

func TestParseWebhookEvents(t *testing.T) {
	c := newConn(t, newFake("created"))
	cases := []struct {
		file string
		kind connector.NotificationKind
		raw  string
		chk  func(t *testing.T, n connector.Notification)
	}{
		{"event_payment_captured", connector.NotifyPayment, "payment.captured", func(t *testing.T, n connector.Notification) {
			p := n.Payment
			if p.Status != connector.PaymentSucceeded || p.Amount != 49900 || p.ProviderOrderID != "order_Nx0" || p.Fee != 1178 || p.Tax != 180 {
				t.Errorf("payment = %+v", p)
			}
		}},
		{"event_payment_authorized", connector.NotifyPayment, "payment.authorized", func(t *testing.T, n connector.Notification) {
			if n.Payment.Status != connector.PaymentAuthorized {
				t.Errorf("payment = %+v", n.Payment)
			}
		}},
		{"event_payment_failed", connector.NotifyPayment, "payment.failed", func(t *testing.T, n connector.Notification) {
			if n.Payment.Status != connector.PaymentFailed || n.Payment.ErrorCode != "BAD_REQUEST_ERROR" {
				t.Errorf("payment = %+v", n.Payment)
			}
		}},
		{"event_order_paid", connector.NotifyPayment, "order.paid", func(t *testing.T, n connector.Notification) {
			if n.Payment.Status != connector.PaymentSucceeded || n.Payment.ProviderOrderID != "order_Nx0" {
				t.Errorf("payment = %+v", n.Payment)
			}
		}},
		{"event_refund_processed", connector.NotifyRefund, "refund.processed", func(t *testing.T, n connector.Notification) {
			r := n.Refund
			if r.Status != connector.RefundSucceeded || r.Amount != 10000 || r.ProviderRefundID != "rfnd_Nx2" || r.ProviderPaymentID != "pay_Nx1" || r.ProviderRef != "rf_01" {
				t.Errorf("refund = %+v", r)
			}
		}},
		{"event_refund_failed", connector.NotifyRefund, "refund.failed", func(t *testing.T, n connector.Notification) {
			if n.Refund.Status != connector.RefundFailed || n.Refund.RawStatus != "failed" {
				t.Errorf("refund = %+v", n.Refund)
			}
		}},
		{"event_dispute_created", connector.NotifyDispute, "payment.dispute.created", func(t *testing.T, n connector.Notification) {
			d := n.Dispute
			if d.Status != connector.DisputeNeedsResponse || d.DueBy != 1761000000000 || d.ProviderPaymentID != "pay_Nx1" || d.Amount != 49900 || d.Phase != "chargeback" {
				t.Errorf("dispute = %+v", d)
			}
		}},
		{"event_dispute_won", connector.NotifyDispute, "payment.dispute.won", func(t *testing.T, n connector.Notification) {
			if n.Dispute.Status != connector.DisputeWon {
				t.Errorf("dispute = %+v", n.Dispute)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			ns, err := c.ParseWebhook(context.Background(), webhookReq(t, tc.file, testWebhookSecret, "evt_42"))
			if err != nil || len(ns) != 1 {
				t.Fatalf("ns=%v err=%v", ns, err)
			}
			n := ns[0]
			if n.Kind != tc.kind || n.EventType != tc.raw || !strings.HasPrefix(n.EventID, "body-") || n.OccurredAt != 1760000100000 {
				t.Fatalf("notification = %+v", n)
			}
			tc.chk(t, n)
		})
	}
}

func TestParseWebhookIgnoresUnknownEventsAndRejectsGarbage(t *testing.T) {
	c := newConn(t, newFake("created"))
	ignored := connector.WebhookRequest{Body: []byte(`{"entity":"event","event":"account.instantly_activated","payload":{}}`)}
	ns, err := c.ParseWebhook(context.Background(), ignored)
	if err != nil || len(ns) != 0 {
		t.Fatalf("ns=%v err=%v", ns, err)
	}
	if _, err := c.ParseWebhook(context.Background(), connector.WebhookRequest{Body: []byte("nope")}); err == nil {
		t.Error("garbage parsed")
	}
}

func TestParseWebhookEventIDFallback(t *testing.T) {
	c := newConn(t, newFake("created"))
	r := webhookReq(t, "event_payment_captured", testWebhookSecret, "")
	delete(r.Headers, "X-Razorpay-Event-Id")
	a, _ := c.ParseWebhook(context.Background(), r)
	b, _ := c.ParseWebhook(context.Background(), r)
	if len(a) != 1 || a[0].EventID == "" || a[0].EventID != b[0].EventID {
		t.Fatalf("fallback id unstable: %v %v", a, b)
	}
}

func TestVerifyWebhookHeaderCaseInsensitive(t *testing.T) {
	c := newConn(t, newFake("created"))
	r := webhookReq(t, "event_payment_captured", testWebhookSecret, "e")
	r.Headers = map[string][]string{"x-razorpay-signature": r.Headers["X-Razorpay-Signature"]}
	if err := c.VerifyWebhook(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Headers["x-razorpay-signature"] = []string{"zz-not-hex"}
	if err := c.VerifyWebhook(context.Background(), r); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Fatalf("err = %v", err)
	}
}

func TestRefundIdempotencyAndMapping(t *testing.T) {
	f := newFake("captured")
	f.refundStatus = "pending"
	c := newConn(t, f).(connector.Refunder)
	snap, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_01", ProviderPaymentID: "pay_Nx1", Amount: 10000, Currency: "INR", Reason: "customer"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.RefundPending || snap.RawStatus != "pending" || snap.ProviderRefundID != "rfnd_Nx2" || snap.ProviderRef != "rf_01" {
		t.Errorf("snap = %+v", snap)
	}
	rc := f.callsTo("POST", "/v1/payments/pay_Nx1/refund")[0]
	var body struct {
		Amount  int64  `json:"amount"`
		Receipt string `json:"receipt"`
	}
	_ = json.Unmarshal(rc.Body, &body)
	if body.Amount != 10000 || body.Receipt != "rf_01" || rc.Header.Get("X-Refund-Idempotency") != "rf_01" {
		t.Errorf("refund request = %s %v", rc.Body, rc.Header)
	}
}

func TestRefundErrors(t *testing.T) {
	for _, tc := range []struct {
		code int
		want func(error) bool
	}{
		{400, func(e error) bool { var nr connector.ErrNotRefundable; return errors.As(e, &nr) && nr.Reason != "" }},
		{401, func(e error) bool { return errors.Is(e, connector.ErrInvalidCredentials) }},
		{404, func(e error) bool { return errors.Is(e, connector.ErrNotFound) }},
		{500, func(e error) bool { return errors.Is(e, connector.ErrTemporary) }},
	} {
		f := newFake("captured")
		f.refundErrCode = tc.code
		c := newConn(t, f).(connector.Refunder)
		_, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf", ProviderPaymentID: "pay_Nx1", Amount: 1, Currency: "INR"})
		if err == nil || !tc.want(err) {
			t.Errorf("code %d: err = %v", tc.code, err)
		}
	}
}

func TestRateLimitedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c, _ := New(testConfig, connectortest.NewDeps(srv, time.Now()))
	_, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_Nx1"})
	var rl connector.ErrRateLimited
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchRefund(t *testing.T) {
	c := newConn(t, newFake("captured")).(connector.Refunder)
	a, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "rfnd_Nx2"})
	if err != nil || a.Status != connector.RefundSucceeded {
		t.Fatalf("a=%+v err=%v", a, err)
	}
	b, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRef: "rf_01", ProviderPaymentID: "pay_Nx1"})
	if err != nil || b.ProviderRefundID != "rfnd_Nx2" {
		t.Fatalf("b=%+v err=%v", b, err)
	}
	if _, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRef: "nope", ProviderPaymentID: "pay_Nx1"}); !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestListerPaginates(t *testing.T) {
	f := newFake("captured")
	f.listPages = 1
	c := newConn(t, f).(connector.Lister)
	tr := connector.TimeRange{From: 1760000000000, To: 1760100000000}
	p1, err := c.ListPayments(context.Background(), tr, "")
	if err != nil || len(p1.Items) != listPageSize || p1.Next == "" {
		t.Fatalf("page1 len=%d next=%q err=%v", len(p1.Items), p1.Next, err)
	}
	p2, err := c.ListPayments(context.Background(), tr, p1.Next)
	if err != nil || len(p2.Items) != 1 || p2.Next != "" {
		t.Fatalf("page2 len=%d next=%q err=%v", len(p2.Items), p2.Next, err)
	}
	q := f.callsTo("GET", "/v1/payments")[0].Query
	if !strings.Contains(q, "from=1760000000") || !strings.Contains(q, "to=1760099999") {
		t.Errorf("query = %q", q)
	}
	r, err := c.ListRefunds(context.Background(), tr, "")
	if err != nil || len(r.Items) == 0 || r.Items[0].Status != connector.RefundSucceeded {
		t.Fatalf("refunds %+v err=%v", r, err)
	}
	if _, err := c.ListPayments(context.Background(), tr, "garbage"); err == nil {
		t.Error("bad cursor accepted")
	}
}

func TestDisputeReader(t *testing.T) {
	c := newConn(t, newFake("captured")).(connector.DisputeReader)
	d, err := c.FetchDispute(context.Background(), connector.DisputeRef{ProviderDisputeID: "disp_Nx3"})
	if err != nil || d.Status != connector.DisputeUnderReview || d.RawStatus != "under_review" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	pg, err := c.ListDisputes(context.Background(), connector.TimeRange{From: 1, To: 1760100000000}, "")
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Status != connector.DisputeNeedsResponse {
		t.Fatalf("pg=%+v err=%v", pg, err)
	}
}

func TestInfo(t *testing.T) {
	c := newConn(t, newFake("created"))
	info := c.Info()
	if info.Name != "razorpay" || !info.Has(connector.CapRefund) || !info.Has(connector.CapList) || !info.Has(connector.CapDisputeRead) {
		t.Errorf("info = %+v", info)
	}
	if !info.Has(connector.CapDisputeRespond) || !info.Has(connector.CapSettle) {
		t.Error("DisputeResponder (accept, contest) and Settler are implemented")
	}
	var schema map[string]any
	if err := json.Unmarshal(info.ConfigSchema, &schema); err != nil {
		t.Fatal(err)
	}
}

func TestStatusMapTotal(t *testing.T) {
	for raw, want := range map[string]connector.PaymentStatus{
		"created": connector.PaymentProcessing, "authorized": connector.PaymentAuthorized, "captured": connector.PaymentSucceeded,
		"refunded": connector.PaymentSucceeded, "failed": connector.PaymentFailed,
	} {
		if got := mapPaymentStatus(raw); got != want {
			t.Errorf("%s -> %s want %s", raw, got, want)
		}
	}
	if mapPaymentStatus("brand_new") != connector.PaymentProcessing {
		t.Error("unknown payment status must map to processing")
	}
	if mapRefundStatus("weird") != connector.RefundPending || mapDisputeStatus("weird") != connector.DisputeUnderReview {
		t.Error("unknown statuses must map to the safe in-flight value")
	}
}

// A payment id that arrives from outside (a return URL, a notification) is
// only a pointer: the payment is used, and captured, only if it belongs to the
// order or receipt we are asking about.
func TestFetchPaymentByIDMustBelongToTheOrderWeAskAbout(t *testing.T) {
	f := newFake("authorized")
	c := newConn(t, f)
	_, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_Nx1", ProviderOrderID: "order_of_somebody_else", ProviderRef: "pay_ref_01"})
	if !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("a payment of another order: %v", err)
	}
	if n := len(f.callsTo("POST", "/v1/payments/pay_Nx1/capture")); n != 0 {
		t.Errorf("a payment that is not ours was captured (%d capture calls)", n)
	}
	// By receipt only: the payment's order must carry our receipt.
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_Nx1", ProviderRef: "another_receipt"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("a payment of an order with another receipt: %v", err)
	}
	if n := len(f.callsTo("POST", "/v1/payments/pay_Nx1/capture")); n != 0 {
		t.Errorf("captured through a wrong receipt (%d capture calls)", n)
	}
	// Ours: found, and captured.
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_Nx1", ProviderOrderID: "order_Nx0", ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentSucceeded || snap.ProviderOrderID != "order_Nx0" {
		t.Errorf("our payment: %+v %v", snap, err)
	}
	// With nothing to compare against, a payment id still works (a dispute lookup, say).
	f2 := newFake("captured")
	if snap, err := newConn(t, f2).FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_Nx1"}); err != nil || snap.Status != connector.PaymentSucceeded {
		t.Errorf("a bare payment id: %+v %v", snap, err)
	}
}

// Razorpay's order list takes a receipt filter; if it is ever ignored, the
// first order of the list must not be taken for ours.
func TestFetchPaymentByReceiptChecksTheReceiptOfTheOrderFound(t *testing.T) {
	f := newFake("captured")
	f.listReceipt = "somebody_elses_receipt"
	c := newConn(t, f)
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("an order of another receipt: %v", err)
	}
	f.listReceipt = ""
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentSucceeded {
		t.Errorf("the right receipt: %+v %v", snap, err)
	}
}
