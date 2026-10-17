package cashfree

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

const (
	testID     = "cf_app_id"
	testSecret = "cf_client_secret_known"
	hookSecret = "whsec_cf_known"
	nowUnix    = int64(1760000100)
)

var testConfig = json.RawMessage(`{"client_id":"` + testID + `","client_secret":"` + testSecret + `","webhook_secret":"` + hookSecret + `","sandbox":true}`)

type call struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

func (c call) json(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("body %q: %v", c.Body, err)
	}
	return m
}

// fake is a Cashfree API; routes are "METHOD /pg/path".
type fake struct {
	mu     sync.Mutex
	calls  []call
	routes map[string]func(w http.ResponseWriter, c call)
}

func newFake() *fake { return &fake{routes: map[string]func(http.ResponseWriter, call){}} }

func (f *fake) on(route string, fn func(http.ResponseWriter, call)) { f.routes[route] = fn }

func (f *fake) reply(route string, status int, body any) {
	f.on(route, func(w http.ResponseWriter, _ call) { write(w, status, body) })
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	fn := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if r.Header.Get("x-client-id") != testID || r.Header.Get("x-client-secret") != testSecret || r.Header.Get("x-api-version") == "" {
		write(w, 401, map[string]any{"type": "authentication_error", "code": "request_failed", "message": "authentication Failed"})
		return
	}
	if fn == nil {
		write(w, 404, map[string]any{"type": "invalid_request_error", "code": "route_not_found", "message": "no route " + r.URL.Path})
		return
	}
	fn(w, c)
}

func (f *fake) to(route string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method+" "+c.Path == route {
			out = append(out, c)
		}
	}
	return out
}

func (f *fake) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newConn(t testing.TB, h http.Handler) connector.Connector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func hook(body []byte, secret string) connector.WebhookRequest {
	ts := strconv.FormatInt(nowUnix*1000, 10)
	return connector.WebhookRequest{Method: "POST", Body: body, ReceivedAt: nowUnix * 1000,
		Headers: map[string][]string{"X-Webhook-Signature": {sign(secret, ts, body)}, "X-Webhook-Timestamp": {ts}}}
}

func orderJSON(id, status string) map[string]any {
	return map[string]any{"cf_order_id": 2149460581, "order_id": id, "order_status": status, "order_amount": 499.0, "order_currency": "INR", "payment_session_id": "session_abc"}
}

func paymentJSON(order string, cf int64, status string, amt float64, at string) map[string]any {
	return map[string]any{"cf_payment_id": cf, "order_id": order, "payment_status": status, "payment_amount": amt, "payment_currency": "INR", "payment_time": at, "payment_group": "upi"}
}

// ---- golden webhook signature, computed independently with Python ----

func TestGoldenSignature(t *testing.T) {
	if got := sign(hookSecret, "1760000000", fixture(t, "event_payment_success")); got != "ID3HQqOYWiVgmJ5XPyNLK3Jv1Tn7eCiJAtcaXAEuIZY=" {
		t.Fatalf("signature drifted: %s", got)
	}
}

func TestVerifyWebhook(t *testing.T) {
	c := newConn(t, newFake())
	body := fixture(t, "event_payment_success")
	ctx := context.Background()
	if err := c.VerifyWebhook(ctx, hook(body, hookSecret)); err != nil {
		t.Fatal(err)
	}
	bad := hook(body, hookSecret)
	bad.Body = append([]byte(" "), body...)
	notB64 := hook(body, hookSecret)
	notB64.Headers["X-Webhook-Signature"] = []string{"%%%"}
	noTS := hook(body, hookSecret)
	delete(noTS.Headers, "X-Webhook-Timestamp")
	otherTS := hook(body, hookSecret)
	otherTS.Headers["X-Webhook-Timestamp"] = []string{"1"}
	for name, req := range map[string]connector.WebhookRequest{"wrong secret": hook(body, "nope"), "tampered body": bad, "not base64": notB64, "no timestamp": noTS, "other timestamp": otherTS, "no headers": {Body: body}} {
		if err := c.VerifyWebhook(ctx, req); !errors.Is(err, connector.ErrInvalidSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWebhookSecretDefaultsToClientSecret(t *testing.T) {
	c, err := New(json.RawMessage(`{"client_id":"a","client_secret":"s3cret"}`), connectortest.NewDeps(nil, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	body := fixture(t, "event_payment_success")
	if err := c.VerifyWebhook(context.Background(), hook(body, "s3cret")); err != nil {
		t.Fatal(err)
	}
}

func TestParseWebhook(t *testing.T) {
	c := newConn(t, newFake())
	ctx := context.Background()
	cases := []struct {
		file   string
		status connector.PaymentStatus
		code   string
	}{
		{"event_payment_success", connector.PaymentSucceeded, ""},
		{"event_payment_failed", connector.PaymentFailed, "GATEWAY_ERROR"},
		{"event_payment_user_dropped", connector.PaymentCanceled, ""},
	}
	for _, tc := range cases {
		ns, err := c.ParseWebhook(ctx, hook(fixture(t, tc.file), hookSecret))
		if err != nil || len(ns) != 1 {
			t.Fatalf("%s: %v %v", tc.file, ns, err)
		}
		if err := connectortest.CheckNotifications(ns); err != nil {
			t.Fatal(err)
		}
		p := ns[0].Payment
		if p == nil || p.Status != tc.status || p.ProviderRef != "pay_ref_01" || p.Amount != 49900 || p.ErrorCode != tc.code || !strings.HasPrefix(p.ProviderPaymentID, "pay_ref_01:") {
			t.Errorf("%s: %+v", tc.file, p)
		}
	}
	ns, err := c.ParseWebhook(ctx, hook(fixture(t, "event_refund_status"), hookSecret))
	if err != nil || len(ns) != 1 || ns[0].Refund == nil {
		t.Fatal(ns, err)
	}
	r := ns[0].Refund
	if r.Status != connector.RefundSucceeded || r.ProviderRef != "rf_ref_01" || r.Amount != 10050 || r.ProviderPaymentID != "pay_ref_01:5114911111111" {
		t.Errorf("%+v", r)
	}
	// Retried deliveries dedupe on the derived event id; different events do not.
	a, _ := c.ParseWebhook(ctx, hook(fixture(t, "event_payment_success"), hookSecret))
	b, _ := c.ParseWebhook(ctx, hook(fixture(t, "event_payment_success"), hookSecret))
	d, _ := c.ParseWebhook(ctx, hook(fixture(t, "event_payment_failed"), hookSecret))
	if a[0].EventID != b[0].EventID || a[0].EventID == d[0].EventID {
		t.Errorf("event ids: %s %s %s", a[0].EventID, b[0].EventID, d[0].EventID)
	}
	if ns, err := c.ParseWebhook(ctx, hook(fixture(t, "event_unknown"), hookSecret)); ns != nil || err != nil {
		t.Errorf("unknown event: %v %v", ns, err)
	}
	if _, err := c.ParseWebhook(ctx, connector.WebhookRequest{Body: []byte(`{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{}}`)}); err == nil {
		t.Error("payment event without a payment must fail")
	}
}

// ---- checkout ----

func checkoutReq() connector.CheckoutRequest {
	return connector.CheckoutRequest{PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01", Amount: 49900, Currency: "INR",
		Description: "Order", Customer: connector.Customer{Email: "asha@example.com", Phone: "+91 98765 43210", Name: "Asha"}}
}

func TestCreateCheckout(t *testing.T) {
	f := newFake()
	f.on("POST /pg/orders", func(w http.ResponseWriter, c call) { write(w, 200, orderJSON("pay_ref_01", "ACTIVE")) })
	c := newConn(t, f)
	res, err := c.CreateCheckout(context.Background(), checkoutReq())
	if err != nil {
		t.Fatal(err)
	}
	na := res.NextAction
	if res.ProviderOrderID != "pay_ref_01" || na.Kind != connector.ActionSDK || na.ScriptURL != sdkScript || na.Options["payment_session_id"] != "session_abc" || na.Options["mode"] != "sandbox" {
		t.Errorf("%+v", res)
	}
	calls := f.to("POST /pg/orders")
	if len(calls) != 1 {
		t.Fatal(len(calls))
	}
	in := calls[0].json(t)
	if in["order_id"] != "pay_ref_01" || in["order_amount"] != 499.0 || in["order_currency"] != "INR" {
		t.Errorf("%v", in)
	}
	cust := in["customer_details"].(map[string]any)
	if cust["customer_phone"] != "9876543210" || cust["customer_email"] != "asha@example.com" {
		t.Errorf("%v", cust)
	}
	meta := in["order_meta"].(map[string]any)
	if !strings.Contains(meta["return_url"].(string), "order_id={order_id}") || !strings.HasSuffix(meta["notify_url"].(string), "/hooks/acc") {
		t.Errorf("%v", meta)
	}
	if calls[0].Header.Get("x-idempotency-key") != "order:pay_ref_01" || calls[0].Header.Get("x-api-version") != apiVersion {
		t.Errorf("%v", calls[0].Header)
	}
}

func TestCreateCheckoutRejects(t *testing.T) {
	f := newFake()
	f.on("POST /pg/orders", func(w http.ResponseWriter, c call) { write(w, 200, orderJSON("some_other_order", "ACTIVE")) })
	c := newConn(t, f)
	ctx := context.Background()
	if _, err := c.CreateCheckout(ctx, checkoutReq()); err == nil {
		t.Error("an order with another id must be refused")
	}
	for name, mut := range map[string]func(*connector.CheckoutRequest){
		"usd":      func(r *connector.CheckoutRequest) { r.Currency = "USD" },
		"zero":     func(r *connector.CheckoutRequest) { r.Amount = 0 },
		"bad ref":  func(r *connector.CheckoutRequest) { r.ProviderRef = "a b/c" },
		"long ref": func(r *connector.CheckoutRequest) { r.ProviderRef = strings.Repeat("a", 51) },
	} {
		r := checkoutReq()
		mut(&r)
		before := f.total()
		if _, err := c.CreateCheckout(ctx, r); err == nil || f.total() != before {
			t.Errorf("%s: err=%v requests=%d", name, err, f.total()-before)
		}
	}
}

func TestHandleReturn(t *testing.T) {
	c := newConn(t, newFake())
	ok, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "pay_ref_01", Query: map[string][]string{"order_id": {"pay_ref_01"}}})
	if err != nil || ok.ProviderRef != "pay_ref_01" {
		t.Fatal(ok, err)
	}
	_, err = c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "pay_ref_01", Query: map[string][]string{"order_id": {"someone_else"}}})
	if !errors.Is(err, connector.ErrInvalidReturn) {
		t.Fatal(err)
	}
}

// ---- strict payment matching ----

func TestFetchPaymentStrict(t *testing.T) {
	f := newFake()
	f.reply("GET /pg/orders/pay_ref_01", 200, orderJSON("pay_ref_01", "ACTIVE"))
	f.reply("GET /pg/orders/pay_ref_01/payments", 200, []any{
		paymentJSON("another_order", 9, "SUCCESS", 499, "2026-10-17T10:00:00+05:30"), // a payment of another order must be ignored
		paymentJSON("pay_ref_01", 1, "FAILED", 499, "2026-10-17T10:01:00+05:30"),
		paymentJSON("pay_ref_01", 2, "SUCCESS", 499, "2026-10-17T10:02:00+05:30"),
		paymentJSON("pay_ref_01", 3, "USER_DROPPED", 499, "2026-10-17T10:03:00+05:30"),
	})
	c := newConn(t, f)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.PaymentSucceeded || snap.ProviderPaymentID != "pay_ref_01:2" || snap.Amount != 49900 {
		t.Errorf("%+v", snap)
	}
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01", ProviderOrderID: "different"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("order/ref mismatch: %v", err)
	}
}

func TestFetchPaymentOnlyForeignPaymentsReportsTheOrder(t *testing.T) {
	f := newFake()
	f.reply("GET /pg/orders/pay_ref_01", 200, orderJSON("pay_ref_01", "ACTIVE"))
	f.reply("GET /pg/orders/pay_ref_01/payments", 200, []any{paymentJSON("another_order", 9, "SUCCESS", 499, "")})
	snap, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.ProviderPaymentID != "" {
		t.Fatalf("%+v %v", snap, err)
	}
}

func TestFetchPaymentWrongOrderIsNotFound(t *testing.T) {
	f := newFake()
	f.reply("GET /pg/orders/pay_ref_01", 200, orderJSON("someone_else", "PAID"))
	f.reply("GET /pg/orders/pay_ref_01/payments", 200, []any{})
	if _, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := newConn(t, f).FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "../x"}); err == nil {
		t.Fatal("an unsafe ref must not reach the API")
	}
}

func TestFetchPaymentByCompositeID(t *testing.T) {
	f := newFake()
	f.reply("GET /pg/orders/pay_ref_01/payments/77", 200, paymentJSON("pay_ref_01", 77, "SUCCESS", 499, "2026-10-17T10:02:00+05:30"))
	f.reply("GET /pg/orders/pay_ref_01/payments/78", 200, paymentJSON("another", 78, "SUCCESS", 499, ""))
	c := newConn(t, f)
	ctx := context.Background()
	snap, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: "pay_ref_01:77", ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentSucceeded {
		t.Fatal(snap, err)
	}
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: "pay_ref_01:78"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("a payment of another order: %v", err)
	}
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: "pay_ref_01:77", ProviderRef: "other"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("ref disagrees: %v", err)
	}
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: "77"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("bare id: %v", err)
	}
}

func TestStatusMapping(t *testing.T) {
	for raw, want := range map[string]connector.PaymentStatus{
		"SUCCESS": connector.PaymentSucceeded, "FAILED": connector.PaymentFailed, "USER_DROPPED": connector.PaymentCanceled, "CANCELLED": connector.PaymentCanceled,
		"VOID": connector.PaymentCanceled, "PENDING": connector.PaymentProcessing, "NOT_ATTEMPTED": connector.PaymentRequiresAction, "BRAND_NEW": connector.PaymentProcessing,
	} {
		if got := mapPaymentStatus(raw); got != want {
			t.Errorf("%s: %s want %s", raw, got, want)
		}
	}
	for raw, want := range map[string]connector.PaymentStatus{"ACTIVE": connector.PaymentRequiresAction, "EXPIRED": connector.PaymentExpired, "TERMINATED": connector.PaymentCanceled, "PAID": connector.PaymentSucceeded, "NEW": connector.PaymentProcessing} {
		if got := mapOrderStatus(raw); got != want {
			t.Errorf("order %s: %s want %s", raw, got, want)
		}
	}
}

func TestErrorClassification(t *testing.T) {
	f := newFake()
	f.reply("GET /pg/orders/a1", 429, map[string]any{"code": "rate_limit_error"})
	f.reply("GET /pg/orders/a2", 502, map[string]any{})
	f.reply("GET /pg/orders/a3", 404, map[string]any{"code": "order_not_found"})
	c := newConn(t, f)
	ctx := context.Background()
	var rl connector.ErrRateLimited
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "a1"}); !errors.As(err, &rl) {
		t.Errorf("429: %v", err)
	}
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "a2"}); !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("502: %v", err)
	}
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "a3"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	bad, _ := New(json.RawMessage(`{"client_id":"x","client_secret":"y"}`), connectortest.NewDeps(httptest.NewServer(f), time.Unix(nowUnix, 0)))
	if _, err := bad.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "a1"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("401: %v", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	for _, cfg := range []string{`{}`, `{"client_id":"a"}`, `{"client_secret":"b"}`, `nope`} {
		if _, err := New(json.RawMessage(cfg), connectortest.NewDeps(nil, time.Unix(nowUnix, 0))); err == nil {
			t.Errorf("%s accepted", cfg)
		}
	}
	prod, err := New(json.RawMessage(`{"client_id":"a","client_secret":"b"}`), connectortest.NewDeps(nil, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if prod.(*cashfree).api.base != prodBase {
		t.Error("prod host expected without the sandbox flag")
	}
	if sb, _ := New(testConfig, connectortest.NewDeps(nil, time.Unix(nowUnix, 0))); sb.(*cashfree).api.base != sandboxBase {
		t.Error("sandbox host expected with the sandbox flag")
	}
}

// ---- refunds ----

func TestRefund(t *testing.T) {
	f := newFake()
	f.on("POST /pg/orders/pay_ref_01/refunds", func(w http.ResponseWriter, c call) {
		write(w, 200, map[string]any{"cf_refund_id": 55501, "cf_payment_id": 77, "refund_id": "rf_ref_01", "order_id": "pay_ref_01", "refund_amount": 100.5, "refund_currency": "INR", "refund_status": "PENDING"})
	})
	c := newConn(t, f).(connector.Refunder)
	snap, err := c.Refund(context.Background(), connector.RefundRequest{ProviderPaymentID: "pay_ref_01:77", ProviderRef: "rf_ref_01", Amount: 10050, Currency: "INR", Reason: "customer request"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.RefundPending || snap.ProviderRef != "rf_ref_01" || snap.Amount != 10050 || snap.ProviderPaymentID != "pay_ref_01:77" {
		t.Errorf("%+v", snap)
	}
	calls := f.to("POST /pg/orders/pay_ref_01/refunds")
	in := calls[0].json(t)
	if in["refund_id"] != "rf_ref_01" || in["refund_amount"] != 100.5 || calls[0].Header.Get("x-idempotency-key") != "rf_ref_01" {
		t.Errorf("%v %v", in, calls[0].Header)
	}
}

func TestRefundRejects(t *testing.T) {
	f := newFake()
	f.on("POST /pg/orders/pay_ref_01/refunds", func(w http.ResponseWriter, c call) {
		write(w, 400, map[string]any{"type": "invalid_request_error", "code": "refund_amount_invalid", "message": "refund amount exceeds the balance"})
	})
	c := newConn(t, f).(connector.Refunder)
	ctx := context.Background()
	_, err := c.Refund(ctx, connector.RefundRequest{ProviderPaymentID: "pay_ref_01:77", ProviderRef: "rf1", Amount: 1, Currency: "INR"})
	var nr connector.ErrNotRefundable
	if !errors.As(err, &nr) {
		t.Fatalf("400: %v", err)
	}
	before := f.total()
	for name, r := range map[string]connector.RefundRequest{
		"long id":    {ProviderPaymentID: "pay_ref_01:77", ProviderRef: strings.Repeat("r", 41), Amount: 1, Currency: "INR"},
		"bare id":    {ProviderPaymentID: "77", ProviderRef: "rf1", Amount: 1, Currency: "INR"},
		"zero":       {ProviderPaymentID: "pay_ref_01:77", ProviderRef: "rf1", Amount: 0, Currency: "INR"},
		"empty id":   {ProviderPaymentID: "pay_ref_01:77", Amount: 1, Currency: "INR"},
		"bad chars":  {ProviderPaymentID: "pay_ref_01:77", ProviderRef: "r/1", Amount: 1, Currency: "INR"},
		"path trick": {ProviderPaymentID: "../x:77", ProviderRef: "rf1", Amount: 1, Currency: "INR"},
	} {
		if _, err := c.Refund(ctx, r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if f.total() != before {
		t.Error("a rejected refund reached the API")
	}
	// A response for a different refund is refused.
	f.on("POST /pg/orders/pay_ref_01/refunds", func(w http.ResponseWriter, c call) {
		write(w, 200, map[string]any{"refund_id": "other", "order_id": "pay_ref_01", "refund_amount": 1, "refund_status": "SUCCESS"})
	})
	if _, err := c.Refund(ctx, connector.RefundRequest{ProviderPaymentID: "pay_ref_01:77", ProviderRef: "rf1", Amount: 100, Currency: "INR"}); err == nil {
		t.Error("a response for another refund_id must be refused")
	}
}

func TestFetchRefundStrict(t *testing.T) {
	f := newFake()
	f.reply("GET /pg/orders/pay_ref_01/refunds/rf_ref_01", 200, map[string]any{"cf_payment_id": 77, "refund_id": "rf_ref_01", "order_id": "pay_ref_01", "refund_amount": 100.5, "refund_status": "SUCCESS"})
	f.reply("GET /pg/orders/pay_ref_01/refunds/rf_ref_02", 200, map[string]any{"refund_id": "rf_unrelated", "order_id": "pay_ref_01", "refund_status": "SUCCESS"})
	f.reply("GET /pg/orders/pay_ref_01/refunds/rf_ref_03", 200, map[string]any{"refund_id": "rf_ref_03", "order_id": "another_order", "refund_status": "SUCCESS"})
	c := newConn(t, f).(connector.Refunder)
	ctx := context.Background()
	snap, err := c.FetchRefund(ctx, connector.RefundRef{ProviderRef: "rf_ref_01", ProviderPaymentID: "pay_ref_01:77"})
	if err != nil || snap.Status != connector.RefundSucceeded || snap.Amount != 10050 {
		t.Fatal(snap, err)
	}
	for _, id := range []string{"rf_ref_02", "rf_ref_03"} {
		if _, err := c.FetchRefund(ctx, connector.RefundRef{ProviderRef: id, ProviderPaymentID: "pay_ref_01:77"}); !errors.Is(err, connector.ErrNotFound) {
			t.Errorf("%s: %v", id, err)
		}
	}
	for raw, want := range map[string]connector.RefundStatus{"SUCCESS": connector.RefundSucceeded, "PENDING": connector.RefundPending, "ONHOLD": connector.RefundPending, "CANCELLED": connector.RefundFailed, "FAILED": connector.RefundFailed, "NEW": connector.RefundPending} {
		if mapRefundStatus(raw) != want {
			t.Errorf("%s", raw)
		}
	}
}

// ---- settlements ----

func TestListSettlements(t *testing.T) {
	f := newFake()
	f.on("POST /pg/settlements", func(w http.ResponseWriter, c call) {
		in := c.json(t)
		if cur, _ := in["pagination"].(map[string]any)["cursor"].(string); cur == "" {
			write(w, 200, map[string]any{"cursor": "c2", "data": []any{map[string]any{"cf_settlement_id": 901, "settlement_utr": "UTR1", "status": "SUCCESS", "amount_settled": 480.0, "service_charge": 16.95, "service_tax": 3.05, "settlement_date": "2026-10-18T00:00:00+05:30"}}})
			return
		}
		write(w, 200, map[string]any{"data": []any{map[string]any{"cf_settlement_id": 902, "status": "PENDING", "amount_settled": 10.0}}})
	})
	f.on("POST /pg/settlement/recon", func(w http.ResponseWriter, c call) {
		write(w, 200, map[string]any{"data": []any{
			map[string]any{"event_type": "PAYMENT", "order_id": "pay_ref_01", "cf_payment_id": 77, "event_amount": 499.0, "event_service_charge": 16.95, "event_service_tax": 3.05},
			map[string]any{"event_type": "REFUND", "refund_id": "rf_ref_01", "event_amount": 100.5},
		}})
	})
	got, err := newConn(t, f).(connector.Settler).ListSettlements(context.Background(), connector.TimeRange{From: 1760000000000, To: 1761000000000})
	if err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	s := got[0]
	if s.ProviderSettlementID != "901" || s.UTR != "UTR1" || s.Amount != 48000 || s.Fees != 1695 || s.Tax != 305 || s.Currency != "INR" || len(s.Items) != 2 {
		t.Errorf("%+v", s)
	}
	if s.Items[0].Kind != "payment" || s.Items[0].ProviderPaymentID != "pay_ref_01:77" || s.Items[0].Amount != 49900 || s.Items[1].Kind != "refund" || s.Items[1].Amount != -10050 {
		t.Errorf("%+v", s.Items)
	}
	first := f.to("POST /pg/settlements")[0].json(t)["filters"].(map[string]any)
	if first["start_date"] == nil || first["end_date"] == nil {
		t.Errorf("%v", first)
	}
}

// ---- conformance ----

func providerFor(orderStatus string, payments ...any) http.Handler {
	f := newFake()
	f.reply("GET /pg/orders/pay_ref_01", 200, orderJSON("pay_ref_01", orderStatus))
	f.reply("GET /pg/orders/pay_ref_01/payments", 200, append([]any{}, payments...))
	f.reply("POST /pg/orders", 200, orderJSON("pay_ref_01", "ACTIVE"))
	return f
}

func TestConformance(t *testing.T) {
	var cases []connectortest.StatusCase
	pay := func(status string) any { return paymentJSON("pay_ref_01", 5, status, 499, "2026-10-17T10:02:00+05:30") }
	for _, s := range []struct {
		raw   string
		order string
		pays  []any
		want  connector.PaymentStatus
	}{
		{"ACTIVE", "ACTIVE", nil, connector.PaymentRequiresAction},
		{"EXPIRED", "EXPIRED", nil, connector.PaymentExpired},
		{"TERMINATED", "TERMINATED", nil, connector.PaymentCanceled},
		{"SUCCESS", "PAID", []any{pay("SUCCESS")}, connector.PaymentSucceeded},
		{"FAILED", "ACTIVE", []any{pay("FAILED")}, connector.PaymentFailed},
		{"USER_DROPPED", "ACTIVE", []any{pay("USER_DROPPED")}, connector.PaymentCanceled},
		{"PENDING", "ACTIVE", []any{pay("PENDING")}, connector.PaymentProcessing},
		{"NOT_ATTEMPTED", "ACTIVE", []any{pay("NOT_ATTEMPTED")}, connector.PaymentRequiresAction},
	} {
		cases = append(cases, connectortest.StatusCase{Raw: s.raw, Want: s.want, WantAmount: 49900, Handler: providerFor(s.order, s.pays...)})
	}
	body := fixture(t, "event_payment_success")
	connectortest.Run(t, New, connectortest.Fixtures{
		Config:   testConfig,
		Now:      time.Unix(nowUnix, 0),
		Ref:      connector.PaymentRef{ProviderRef: "pay_ref_01", ProviderOrderID: "pay_ref_01"},
		Statuses: cases,
		Webhooks: connectortest.WebhookFixtures{
			Valid:            hook(body, hookSecret),
			WrongSecret:      hook(body, "whsec_not_the_secret"),
			SignatureHeaders: []string{"x-webhook-signature", "x-webhook-timestamp"},
			// No Stale fixture: the timestamp is signed but no tolerance is applied (see VerifyWebhook).
		},
		// No Return fixtures: Cashfree signs nothing on the redirect.
		Checkout:        checkoutReq(),
		CheckoutHandler: providerFor("ACTIVE"),
	})
}
