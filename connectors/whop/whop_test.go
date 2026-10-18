package whop

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

const (
	testAPIKey = "whop_api_key_test"
	testSecret = "ws_test_known_secret"
	nowUnix    = int64(1760000000)
	// Independent Python hmac+base64 over "msg_2Kx.1760000000." + testdata/payment_succeeded.json.
	goldenSig = "wL/nF/iVFYpwTCuPl8IN5Z7G6bebN3i0jfATg15BOm8="
)

var testConfig = json.RawMessage(`{"api_key":"` + testAPIKey + `","company_id":"biz_1","webhook_secret":"` + testSecret + `"}`)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(secret []byte, id string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s.%d.", id, ts)
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func webhook(t *testing.T, name, id string, secret []byte, ts int64) connector.WebhookRequest {
	t.Helper()
	b := fixture(t, name)
	return connector.WebhookRequest{
		Method: "POST", Body: b, ReceivedAt: nowUnix * 1000,
		Headers: map[string][]string{
			"Webhook-Id": {id}, "Webhook-Timestamp": {fmt.Sprint(ts)}, "Webhook-Signature": {sign(secret, id, ts, b)},
		},
	}
}

func TestGoldenSignature(t *testing.T) {
	got := sign([]byte(testSecret), "msg_2Kx", nowUnix, fixture(t, "payment_succeeded"))
	if got != "v1,"+goldenSig {
		t.Fatalf("signature drifted: %s", got)
	}
}

// fake is a Whop v1 API.
type fake struct {
	mu           sync.Mutex
	substatus    string // "" = no payment yet
	refunded     string
	calls        []string
	bodies       map[string][]byte
	hdr          map[string]http.Header
	refundErr    int
	memberStatus string
	noOwner      bool // pay_w1 names no checkout configuration and no reference
}

func newFake(sub string) *fake {
	return &fake{substatus: sub, refunded: "0", bodies: map[string][]byte{}, hdr: map[string]http.Header{}}
}

func (f *fake) payment() map[string]any {
	status := map[string]string{"succeeded": "paid", "refunded": "paid", "failed": "failed", "pending": "open", "draft": "draft"}[f.substatus]
	if f.noOwner {
		return map[string]any{"id": "pay_w1", "status": status, "substatus": f.substatus, "total": 4.99, "currency": "usd",
			"refunded_amount": json.Number(f.refunded), "payment_method_type": "card", "created_at": "2026-10-17T11:59:00Z"}
	}
	return map[string]any{"id": "pay_w1", "status": status, "substatus": f.substatus, "total": 4.99, "currency": "usd",
		"checkout_configuration_id": "ch_1", "refunded_amount": json.Number(f.refunded), "payment_method_type": "card",
		"metadata": map[string]string{"ref": "pay_ref_01"}, "created_at": "2026-10-17T11:59:00Z"}
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.bodies[key], f.hdr[key] = b, r.Header.Clone()
	if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
		write(w, 401, map[string]any{"error": map[string]any{"message": "bad key"}})
		return
	}
	switch key {
	case "POST /api/v1/checkout_configurations":
		write(w, 200, map[string]any{"id": "ch_1", "purchase_url": "https://whop.com/checkout/ch_1", "plan": map[string]any{"id": "plan_1"}})
	case "GET /api/v1/payments":
		if r.URL.Query().Get("checkout_configuration_id") != "ch_1" {
			write(w, 200, map[string]any{"data": []any{}})
			return
		}
		var d []any
		if f.substatus != "" {
			d = append(d, f.payment())
		}
		write(w, 200, map[string]any{"data": d})
	case "GET /api/v1/checkout_configurations/ch_1":
		write(w, 200, map[string]any{"id": "ch_1", "plan": map[string]any{"initial_price": 4.99, "currency": "usd"}})
	case "GET /api/v1/payments/pay_w1":
		write(w, 200, f.payment())
	case "POST /api/v1/payments/pay_w1/refund":
		if f.refundErr != 0 {
			write(w, f.refundErr, map[string]any{"error": map[string]any{"message": "already refunded"}})
			return
		}
		write(w, 200, f.payment())
	case "GET /api/v1/memberships/mem_w1":
		write(w, 200, map[string]any{"id": "mem_w1", "status": f.memberStatus, "metadata": map[string]string{"ref": "SUBREF01"},
			"renewal_period_start": "2026-10-17T11:00:00Z", "renewal_period_end": "2026-11-16T11:00:00Z"})
	case "POST /api/v1/memberships/mem_w1/cancel":
		write(w, 200, map[string]any{"id": "mem_w1", "status": "canceled"})
	case "GET /api/v1/refunds/rfnd_w1":
		write(w, 200, map[string]any{"id": "rfnd_w1", "payment_id": "pay_w1", "amount": 1.5, "currency": "usd", "status": "succeeded"})
	default:
		write(w, 404, map[string]any{"error": map[string]any{"message": "nope"}})
	}
}

func newConn(t *testing.T, f *fake) connector.Connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConformance(t *testing.T) {
	sts := []struct {
		sub  string
		want connector.PaymentStatus
	}{
		{"succeeded", connector.PaymentSucceeded}, {"failed", connector.PaymentFailed}, {"pending", connector.PaymentProcessing},
		{"refunded", connector.PaymentSucceeded}, {"draft", connector.PaymentRequiresAction},
	}
	var cases []connectortest.StatusCase
	for _, s := range sts {
		cases = append(cases, connectortest.StatusCase{Raw: s.sub, Want: s.want, WantAmount: 499, Handler: newFake(s.sub)})
	}
	stale := webhook(t, "payment_succeeded", "msg_2Kx", []byte(testSecret), nowUnix-3600)
	connectortest.Run(t, New, connectortest.Fixtures{
		Config:   testConfig,
		Now:      time.Unix(nowUnix, 0),
		Ref:      connector.PaymentRef{ProviderPaymentID: "pay_w1"},
		Statuses: cases,
		Webhooks: connectortest.WebhookFixtures{
			Valid:            webhook(t, "payment_succeeded", "msg_2Kx", []byte(testSecret), nowUnix),
			WrongSecret:      webhook(t, "payment_succeeded", "msg_2Kx", []byte("ws_other"), nowUnix),
			SignatureHeaders: []string{"webhook-id", "webhook-timestamp", "webhook-signature"},
			Stale:            &stale,
		},
		Checkout: connector.CheckoutRequest{
			PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01", Amount: 499, Currency: "USD",
		},
		CheckoutHandler: newFake(""),
		ConnectionOK:    newFake(""),
		ConnectionRejected: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			write(w, 401, map[string]any{"error": map[string]any{"message": "bad key"}})
		}),
	})
}

func TestRegisteredAndInfo(t *testing.T) {
	if _, _, ok := connector.Lookup("whop"); !ok {
		t.Fatal("not registered")
	}
	i := newConn(t, newFake("")).Info()
	if !i.MerchantOfRecord || !i.Has(connector.CapRefund) || !i.Has(connector.CapDisputeRespond) || !i.Has(connector.CapDisputeRead) {
		t.Errorf("info = %+v", i)
	}
	has := map[string]bool{}
	for _, c := range i.Currencies {
		has[c] = true
	}
	if !has["BDT"] || !has["USD"] || !has["INR"] || has["JPY"] {
		t.Errorf("currencies = %v", i.Currencies)
	}
}

func TestConfigValidation(t *testing.T) {
	d := connectortest.NewDeps(nil, time.Now())
	for _, cfg := range []string{`{}`, `x`, `{"api_key":"a","company_id":"b"}`} {
		if _, err := New(json.RawMessage(cfg), d); err == nil {
			t.Errorf("config %s accepted", cfg)
		}
	}
}

func TestCreateCheckoutBodyAndRedirect(t *testing.T) {
	f := newFake("")
	c := newConn(t, f)
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{
		PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01", Amount: 12345, Currency: "BDT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderOrderID != "ch_1" || res.NextAction.Kind != connector.ActionRedirect || res.NextAction.URL != "https://whop.com/checkout/ch_1" {
		t.Fatalf("res = %+v", res)
	}
	var body struct {
		Plan struct {
			CompanyID    string      `json:"company_id"`
			PlanType     string      `json:"plan_type"`
			Currency     string      `json:"currency"`
			InitialPrice json.Number `json:"initial_price"`
		} `json:"plan"`
		Metadata    map[string]string `json:"metadata"`
		RedirectURL string            `json:"redirect_url"`
	}
	_ = json.Unmarshal(f.bodies["POST /api/v1/checkout_configurations"], &body)
	if body.Plan.CompanyID != "biz_1" || body.Plan.PlanType != "one_time" || body.Plan.Currency != "bdt" || body.Plan.InitialPrice.String() != "123.45" ||
		body.Metadata["pay"] != "pay_01" || body.Metadata["att"] != "att_01" || body.Metadata["app"] != "app_01" || body.Metadata["ref"] != "pay_ref_01" ||
		body.RedirectURL != "https://pay.example.com/return/acc/pay_ref_01" {
		t.Errorf("body = %s", f.bodies["POST /api/v1/checkout_configurations"])
	}
	for _, r := range []connector.CheckoutRequest{{ProviderRef: "r", Amount: 1, Currency: "ZZZ"}, {ProviderRef: "", Amount: 1, Currency: "USD"}, {ProviderRef: "r", Currency: "USD"}} {
		if _, err := c.CreateCheckout(context.Background(), r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestBadAPIKey(t *testing.T) {
	srv := httptest.NewServer(newFake(""))
	defer srv.Close()
	c, _ := New(json.RawMessage(strings.Replace(string(testConfig), testAPIKey, "wrong", 1)), connectortest.NewDeps(srv, time.Now()))
	if _, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{ProviderRef: "r", Amount: 1, Currency: "USD"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestWebhookToleranceAndFormats(t *testing.T) {
	c := newConn(t, newFake(""))
	secret := []byte(testSecret)
	for _, tc := range []struct {
		off int64
		ok  bool
	}{{0, true}, {-299, true}, {299, true}, {-301, false}, {301, false}} {
		if err := c.VerifyWebhook(context.Background(), webhook(t, "payment_succeeded", "msg_2Kx", secret, nowUnix+tc.off)); (err == nil) != tc.ok {
			t.Errorf("offset %d: %v", tc.off, err)
		}
	}
	// Several signatures: one wrong, one right, plus an unknown version.
	r := webhook(t, "payment_succeeded", "msg_2Kx", secret, nowUnix)
	good := r.Headers["Webhook-Signature"][0]
	r.Headers["Webhook-Signature"] = []string{"v1,AAAA v2,zzzz " + good}
	if err := c.VerifyWebhook(context.Background(), r); err != nil {
		t.Errorf("multi-sig: %v", err)
	}
	// The id is part of the signed content.
	r = webhook(t, "payment_succeeded", "msg_2Kx", secret, nowUnix)
	r.Headers["Webhook-Id"] = []string{"msg_other"}
	if err := c.VerifyWebhook(context.Background(), r); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Errorf("swapped id: %v", err)
	}
	// Garbage timestamp.
	r = webhook(t, "payment_succeeded", "msg_2Kx", secret, nowUnix)
	r.Headers["Webhook-Timestamp"] = []string{"soon"}
	if err := c.VerifyWebhook(context.Background(), r); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Errorf("bad ts: %v", err)
	}
}

func TestSigningKeyRawVersusWhsec(t *testing.T) {
	if string(signingKey("ws_abc")) != "ws_abc" {
		t.Error("ws_ secret must be used raw, prefix included")
	}
	raw := []byte("0123456789abcdef")
	if got := signingKey("whsec_" + base64.StdEncoding.EncodeToString(raw)); string(got) != string(raw) {
		t.Errorf("whsec_ secret not base64-decoded: %q", got)
	}
}

func TestParseWebhook(t *testing.T) {
	c := newConn(t, newFake(""))
	ns, err := c.ParseWebhook(context.Background(), webhook(t, "payment_succeeded", "msg_2Kx", []byte(testSecret), nowUnix))
	if err != nil || len(ns) != 1 {
		t.Fatalf("ns=%v err=%v", ns, err)
	}
	n := ns[0]
	p := n.Payment
	if n.EventID != "msg_2Kx" || n.EventType != "payment.succeeded" || p.Status != connector.PaymentSucceeded || p.Amount != 499 || p.Currency != "USD" ||
		p.ProviderPaymentID != "pay_w1" || p.ProviderOrderID != "ch_1" || p.ProviderRef != "pay_ref_01" || p.RawStatus != "succeeded" {
		t.Errorf("n = %+v p = %+v", n, p)
	}
	if n.OccurredAt != time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("occurredAt = %d", n.OccurredAt)
	}
	ns, _ = c.ParseWebhook(context.Background(), webhook(t, "payment_failed", "msg_2Ky", []byte(testSecret), nowUnix))
	if ns[0].Payment.Status != connector.PaymentFailed {
		t.Errorf("failed = %+v", ns[0].Payment)
	}
	ns, _ = c.ParseWebhook(context.Background(), webhook(t, "refund_created", "msg_2Kz", []byte(testSecret), nowUnix))
	if r := ns[0].Refund; r == nil || r.Status != connector.RefundPending || r.Amount != 150 || r.ProviderPaymentID != "pay_w1" || r.ProviderRefundID != "rfnd_w1" {
		t.Errorf("refund = %+v", ns[0].Refund)
	}
	ns, err = c.ParseWebhook(context.Background(), webhook(t, "membership_activated", "msg_2L0", []byte(testSecret), nowUnix))
	if err != nil || len(ns) != 1 || ns[0].Kind != connector.NotifySubscription {
		t.Errorf("membership: %v %v", ns, err)
	}
}

func TestFetchPaymentByOrder(t *testing.T) {
	f := newFake("")
	c := newConn(t, f)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "ch_1", ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.Amount != 499 || snap.Currency != "USD" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	f.substatus = "succeeded"
	snap, err = c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "ch_1"})
	if err != nil || snap.Status != connector.PaymentSucceeded || snap.ProviderPaymentID != "pay_w1" || snap.ProviderRef != "pay_ref_01" {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{}); err == nil {
		t.Error("empty ref accepted")
	}
}

func TestHandleReturnNeedsNoSignature(t *testing.T) {
	c := newConn(t, newFake(""))
	got, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "r", Query: map[string][]string{"payment_id": {"pay_w1"}}})
	if err != nil || got.ProviderRef != "r" || got.ProviderPaymentID != "pay_w1" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestRefund(t *testing.T) {
	f := newFake("succeeded")
	c := newConn(t, f).(connector.Refunder)
	s, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_01", ProviderPaymentID: "pay_w1", Amount: 150, Currency: "USD"})
	if err != nil || s.Status != connector.RefundPending || s.ProviderRef != "rf_01" || s.ProviderPaymentID != "pay_w1" || s.Amount != 150 {
		t.Fatalf("s=%+v err=%v", s, err)
	}
	key := "POST /api/v1/payments/pay_w1/refund"
	var body map[string]any
	_ = json.Unmarshal(f.bodies[key], &body)
	if fmt.Sprint(body["partial_amount"]) != "1.50" && fmt.Sprint(body["partial_amount"]) != "1.5" {
		t.Errorf("body = %s", f.bodies[key])
	}
	if f.hdr[key].Get("Idempotency-Key") != "rf_01" {
		t.Errorf("headers = %v", f.hdr[key])
	}
	f.refundErr = 422
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "x", ProviderPaymentID: "pay_w1", Amount: 1, Currency: "USD"}); !errors.As(err, &nr) || nr.Reason == "" {
		t.Errorf("err = %v", err)
	}
}

func TestFetchRefund(t *testing.T) {
	f := newFake("succeeded")
	c := newConn(t, f).(connector.Refunder)
	s, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "rfnd_w1", ProviderRef: "rf_01"})
	if err != nil || s.Status != connector.RefundSucceeded || s.Amount != 150 || s.ProviderRef != "rf_01" {
		t.Fatalf("s=%+v err=%v", s, err)
	}
	// Synthetic id: derived from the payment's refunded amount.
	s, err = c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "rf_01", ProviderRef: "rf_01", ProviderPaymentID: "pay_w1"})
	if err != nil || s.Status != connector.RefundPending {
		t.Fatalf("pending: s=%+v err=%v", s, err)
	}
	f.refunded = "1.5"
	s, _ = c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "rf_01", ProviderRef: "rf_01", ProviderPaymentID: "pay_w1"})
	if s.Status != connector.RefundSucceeded || s.Amount != 150 {
		t.Errorf("refunded: %+v", s)
	}
}

// The return URL's payment_id is not signed: a payment id is only used if the
// payment belongs to the checkout or the reference we are asking about.
func TestFetchPaymentByIDMustBelongToOurCheckout(t *testing.T) {
	f := newFake("succeeded") // pay_w1: checkout ch_1, reference pay_ref_01
	c := newConn(t, f)
	ctx := context.Background()
	for name, ref := range map[string]connector.PaymentRef{
		"another checkout":  {ProviderPaymentID: "pay_w1", ProviderOrderID: "ch_somebody_else"},
		"another reference": {ProviderPaymentID: "pay_w1", ProviderRef: "somebody_elses_ref"},
		"both wrong":        {ProviderPaymentID: "pay_w1", ProviderOrderID: "ch_x", ProviderRef: "ref_x"},
	} {
		if _, err := c.FetchPayment(ctx, ref); !errors.Is(err, connector.ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, ref := range map[string]connector.PaymentRef{
		"our checkout":       {ProviderPaymentID: "pay_w1", ProviderOrderID: "ch_1"},
		"our reference":      {ProviderPaymentID: "pay_w1", ProviderRef: "pay_ref_01"},
		"nothing to compare": {ProviderPaymentID: "pay_w1"},
	} {
		if snap, err := c.FetchPayment(ctx, ref); err != nil || snap.ProviderPaymentID != "pay_w1" || snap.Status != connector.PaymentSucceeded {
			t.Errorf("%s: %+v %v", name, snap, err)
		}
	}
	// A payment with no checkout and no reference of ours (bought straight from
	// the company's own page) is not ours just because our reference is asked for.
	f.noOwner = true
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: "pay_w1", ProviderOrderID: "ch_1", ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("a payment that names neither our checkout nor our reference: %v", err)
	}
}
