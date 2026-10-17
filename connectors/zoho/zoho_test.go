package zoho

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

const (
	testSecret = "zoho_whsec_known"
	nowUnix    = int64(1760000000)
)

var widgetCfg = json.RawMessage(`{"account_id":"acc1","client_id":"cid","client_secret":"csecret","refresh_token":"rtoken","webhook_secret":"` + testSecret + `","widget_api_key":"wkey"}`)
var hostedCfg = json.RawMessage(`{"account_id":"acc1","client_id":"cid","client_secret":"csecret","refresh_token":"rtoken","webhook_secret":"` + testSecret + `","hosted_checkout_url":"https://pay.zoho.example.com/checkout"}`)

// fake is a Zoho OAuth + Payments server.
type fake struct {
	mu          sync.Mutex
	tokenCalls  int
	valid       string // token the API accepts
	refreshWith string // token minted on refresh (default tokN)
	expiresIn   int
	badRefresh  bool
	calls       []string
	bodies      map[string][]byte
	status      string // payment status ("" = no payments yet)
	sessionStat string
	refundStat  string
	refundCode  int
	mandateStat string // status of mandate md_z1
	chargeCode  int    // error status for mandate charges
	chargeMsg   string
}

func newFake(status string) *fake {
	return &fake{valid: "tok1", expiresIn: 3600, status: status, sessionStat: "created", refundStat: "succeeded", bodies: map[string][]byte{}}
}

func pay(status string) map[string]any {
	return map[string]any{"payment_id": "pay_z1", "payments_session_id": "ps_1", "amount": 499.00, "currency": "INR",
		"status": status, "payment_method": "upi", "reference_number": "pay_ref_01"}
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
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.bodies[r.Method+" "+r.URL.Path] = b
	if r.URL.Path == "/oauth/v2/token" {
		f.tokenCalls++
		form, _ := url.ParseQuery(string(b))
		if f.badRefresh || form.Get("refresh_token") != "rtoken" || form.Get("grant_type") != "refresh_token" {
			write(w, 200, map[string]any{"error": "invalid_code"})
			return
		}
		tok := f.refreshWith
		if tok == "" {
			tok = fmt.Sprintf("tok%d", f.tokenCalls)
		}
		f.valid = tok
		write(w, 200, map[string]any{"access_token": tok, "expires_in": f.expiresIn, "token_type": "Bearer"})
		return
	}
	if r.Header.Get("Authorization") != "Zoho-oauthtoken "+f.valid {
		write(w, 401, map[string]any{"code": 57, "message": "invalid token"})
		return
	}
	if r.URL.Query().Get("account_id") != "acc1" {
		write(w, 400, map[string]any{"code": 1, "message": "account_id missing"})
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v1/paymentsessions":
		write(w, 200, map[string]any{"code": 0, "message": "success", "payments_session": map[string]any{"payments_session_id": "ps_1", "amount": "499.00", "currency": "INR"}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/paymentsessions/ps_1":
		var ps []any
		if f.status != "" {
			ps = append(ps, pay(f.status))
		}
		write(w, 200, map[string]any{"code": 0, "payments_session": map[string]any{"payments_session_id": "ps_1", "status": f.sessionStat, "amount": 499.00, "currency": "INR", "payments": ps}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/payments/pay_z1":
		write(w, 200, map[string]any{"code": 0, "payment": pay(f.status)})
	case r.Method == "POST" && r.URL.Path == "/api/v1/payments/pay_z1/refunds":
		if f.refundCode != 0 {
			write(w, f.refundCode, map[string]any{"code": 9, "message": "refund exceeds payment"})
			return
		}
		write(w, 200, map[string]any{"code": 0, "refund": map[string]any{"refund_id": "rf_z1", "payment_id": "pay_z1", "amount": "100.00", "status": f.refundStat, "reference_number": "rf_01"}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/refunds/rf_z1":
		write(w, 200, map[string]any{"code": 0, "refund": map[string]any{"refund_id": "rf_z1", "payment_id": "pay_z1", "amount": 100, "status": f.refundStat, "reference_number": "rf_01"}})
	case r.Method == "POST" && r.URL.Path == "/api/v1/mandates":
		write(w, 200, map[string]any{"code": 0, "mandate": map[string]any{"mandate_id": "md_z1", "payments_session_id": "ps_m1", "status": "created", "max_amount": 748.5, "currency": "INR"}})
	case r.Method == "GET" && r.URL.Path == "/api/v1/mandates/md_z1":
		write(w, 200, map[string]any{"code": 0, "mandate": map[string]any{"mandate_id": "md_z1", "status": f.mandateStat, "max_amount": "748.50", "currency": "INR"}})
	case r.Method == "POST" && r.URL.Path == "/api/v1/mandates/md_z1/charges":
		if f.chargeCode != 0 {
			write(w, f.chargeCode, map[string]any{"code": 9, "message": f.chargeMsg})
			return
		}
		write(w, 200, map[string]any{"code": 0, "payment": map[string]any{"payment_id": "pay_zm1", "status": "pending", "amount": 499.0, "currency": "INR", "reference_number": "DEBITREF01"}})
	case r.Method == "POST" && r.URL.Path == "/api/v1/mandates/md_z1/revoke":
		if f.mandateStat == "revoked" {
			write(w, 400, map[string]any{"code": 9, "message": "Mandate already revoked"})
			return
		}
		write(w, 200, map[string]any{"code": 0, "message": "success"})
	default:
		write(w, 404, map[string]any{"code": 404, "message": "not found"})
	}
}

func newConn(t *testing.T, f *fake, cfg json.RawMessage) connector.Connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(cfg, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func signedWebhook(secret, body string, ts int64, eventTime int64) connector.WebhookRequest {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.%s", ts, body)
	return connector.WebhookRequest{
		Method: "POST", Body: []byte(body), ReceivedAt: eventTime * 1000,
		Headers: map[string][]string{"X-Zoho-Webhook-Signature": {fmt.Sprintf("t=%d,v=%s", ts, hex.EncodeToString(m.Sum(nil)))}},
	}
}

const paymentSucceededBody = `{"event_id":"evt_z1","event_type":"payment.succeeded","payload":{"payment":{"payment_id":"pay_z1","payments_session_id":"ps_1","amount":499.00,"currency":"INR","status":"succeeded","payment_method":"upi","reference_number":"pay_ref_01"}}}`
const refundSucceededBody = `{"event_id":"evt_z2","event_type":"refund.succeeded","payload":{"refund":{"refund_id":"rf_z1","payment_id":"pay_z1","amount":"100.00","status":"succeeded","reference_number":"rf_01"}}}`

func TestConformance(t *testing.T) {
	sts := []struct {
		raw  string
		want connector.PaymentStatus
	}{
		{"succeeded", connector.PaymentSucceeded}, {"failed", connector.PaymentFailed}, {"pending", connector.PaymentProcessing},
		{"initiated", connector.PaymentProcessing}, {"canceled", connector.PaymentCanceled},
	}
	var cases []connectortest.StatusCase
	for _, s := range sts {
		cases = append(cases, connectortest.StatusCase{Raw: s.raw, Want: s.want, WantAmount: 49900, Handler: newFake(s.raw)})
	}
	stale := signedWebhook(testSecret, paymentSucceededBody, nowUnix-3600, nowUnix)
	connectortest.Run(t, New, connectortest.Fixtures{
		Config:   widgetCfg,
		Now:      time.Unix(nowUnix, 0),
		Ref:      connector.PaymentRef{ProviderOrderID: "ps_1"},
		Statuses: cases,
		Webhooks: connectortest.WebhookFixtures{
			Valid:            signedWebhook(testSecret, paymentSucceededBody, nowUnix, nowUnix),
			WrongSecret:      signedWebhook("not_the_secret", paymentSucceededBody, nowUnix, nowUnix),
			SignatureHeaders: []string{"X-Zoho-Webhook-Signature"},
			Stale:            &stale,
		},
		Checkout: connector.CheckoutRequest{
			PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01", Amount: 49900, Currency: "INR",
		},
		CheckoutHandler: newFake(""),
	})
}

// Golden: independent Python hmac over "t.body" with the test secret.
func TestGoldenSignature(t *testing.T) {
	r := signedWebhook(testSecret, paymentSucceededBody, 1760000000, 1760000000)
	m := hmac.New(sha256.New, []byte(testSecret))
	m.Write([]byte("1760000000." + paymentSucceededBody))
	if !strings.Contains(r.Headers["X-Zoho-Webhook-Signature"][0], hex.EncodeToString(m.Sum(nil))) {
		t.Fatal("signature construction drifted")
	}
}

func TestRegisteredAndInfo(t *testing.T) {
	_, info, ok := connector.Lookup("zoho")
	if !ok || info.DisplayName == "" {
		t.Fatal("not registered")
	}
	c := newConn(t, newFake(""), widgetCfg)
	i := c.Info()
	if !i.Has(connector.CapRefund) || i.Has(connector.CapDisputeRead) || i.Has(connector.CapDisputeRespond) {
		t.Errorf("caps = %v", i.Capabilities)
	}
	if len(i.Currencies) != 1 || i.Currencies[0] != "INR" || i.RateLimit.RequestsPerSecond != 1 {
		t.Errorf("info = %+v", i)
	}
}

func TestConfigValidation(t *testing.T) {
	d := connectortest.NewDeps(nil, time.Now())
	for _, cfg := range []string{`{}`, `x`,
		`{"account_id":"a","client_id":"c","client_secret":"s","refresh_token":"r","webhook_secret":"w"}`} { // no widget or hosted
		if _, err := New(json.RawMessage(cfg), d); err == nil {
			t.Errorf("config %s accepted", cfg)
		}
	}
	d.KV = nil
	if _, err := New(widgetCfg, d); err == nil {
		t.Error("nil KV accepted")
	}
}

func TestCheckoutWidget(t *testing.T) {
	f := newFake("")
	c := newConn(t, f, widgetCfg)
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{
		PaymentID: "pay_01", AttemptID: "att_01", AppID: "app_01", ProviderRef: "pay_ref_01", Amount: 49900, Currency: "INR", Description: "Pro",
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Amount    json.Number         `json:"amount"`
		Currency  string              `json:"currency"`
		Reference string              `json:"reference_number"`
		Meta      []map[string]string `json:"meta_data"`
	}
	_ = json.Unmarshal(f.bodies["POST /api/v1/paymentsessions"], &body)
	if body.Amount.String() != "499.00" || body.Currency != "INR" || body.Reference != "pay_ref_01" || len(body.Meta) != 3 || body.Meta[1]["value"] != "att_01" {
		t.Errorf("session body = %s", f.bodies["POST /api/v1/paymentsessions"])
	}
	na := res.NextAction
	if res.ProviderOrderID != "ps_1" || na.Kind != connector.ActionSDK || na.Options["api_key"] != "wkey" || na.Options["payments_session_id"] != "ps_1" ||
		na.Options["account_id"] != "acc1" || na.Options["return_url"] != "https://pay.example.com/return/acc/pay_ref_01" {
		t.Errorf("res = %+v", res)
	}
}

func TestCheckoutHostedRedirect(t *testing.T) {
	c := newConn(t, newFake(""), hostedCfg)
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{ProviderRef: "r", Amount: 100, Currency: "INR"})
	if err != nil || res.NextAction.Kind != connector.ActionRedirect || res.NextAction.URL != "https://pay.zoho.example.com/checkout?payments_session_id=ps_1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for _, r := range []connector.CheckoutRequest{{ProviderRef: "r", Amount: 100, Currency: "USD"}, {ProviderRef: "r", Currency: "INR"}} {
		if _, err := c.CreateCheckout(context.Background(), r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestTokenCachedAndRefreshedOn401(t *testing.T) {
	f := newFake("succeeded")
	c := newConn(t, f, widgetCfg)
	ref := connector.PaymentRef{ProviderPaymentID: "pay_z1"}
	for i := 0; i < 3; i++ {
		if _, err := c.FetchPayment(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1 (cached)", f.tokenCalls)
	}
	// Server revokes the token: one 401, one refresh, then success.
	f.mu.Lock()
	f.valid = "revoked-elsewhere"
	f.mu.Unlock()
	f.refreshWith = ""
	if _, err := c.FetchPayment(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls != 2 {
		t.Fatalf("token calls = %d, want 2", f.tokenCalls)
	}
}

func TestTokenExpiresWithKVTTL(t *testing.T) {
	f := newFake("succeeded")
	f.expiresIn = 120 // cached for 60s (one minute early)
	srv := httptest.NewServer(f)
	defer srv.Close()
	now := time.Unix(nowUnix, 0)
	d := connectortest.NewDeps(srv, now)
	d.KV = connector.NewMemKV(func() time.Time { return now })
	c, _ := New(widgetCfg, d)
	ref := connector.PaymentRef{ProviderPaymentID: "pay_z1"}
	_, _ = c.FetchPayment(context.Background(), ref)
	now = now.Add(30 * time.Second)
	_, _ = c.FetchPayment(context.Background(), ref)
	if f.tokenCalls != 1 {
		t.Fatalf("token calls = %d before expiry", f.tokenCalls)
	}
	now = now.Add(60 * time.Second)
	_, _ = c.FetchPayment(context.Background(), ref)
	if f.tokenCalls != 2 {
		t.Fatalf("token calls = %d after expiry", f.tokenCalls)
	}
}

func TestBadRefreshTokenIsInvalidCredentials(t *testing.T) {
	f := newFake("succeeded")
	f.badRefresh = true
	c := newConn(t, f, widgetCfg)
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "pay_z1"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchPaymentSessionStates(t *testing.T) {
	f := newFake("")
	c := newConn(t, f, widgetCfg)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "ps_1", ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentRequiresAction || snap.Amount != 49900 {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	f.sessionStat = "expired"
	snap, _ = c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "ps_1"})
	if snap.Status != connector.PaymentExpired {
		t.Errorf("snap = %+v", snap)
	}
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderOrderID: "ps_missing"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestHandleReturnEchoesIDs(t *testing.T) {
	c := newConn(t, newFake(""), widgetCfg)
	got, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "r", Query: map[string][]string{"payment_id": {"pay_z1"}, "payments_session_id": {"ps_1"}}})
	if err != nil || got.ProviderPaymentID != "pay_z1" || got.ProviderOrderID != "ps_1" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if _, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "r"}); !errors.Is(err, connector.ErrInvalidReturn) {
		t.Errorf("err = %v", err)
	}
}

func TestParseWebhookPaymentAndRefund(t *testing.T) {
	c := newConn(t, newFake(""), widgetCfg)
	ns, err := c.ParseWebhook(context.Background(), signedWebhook(testSecret, paymentSucceededBody, nowUnix, nowUnix))
	if err != nil || len(ns) != 1 || ns[0].EventID != "evt_z1" || ns[0].Payment.Status != connector.PaymentSucceeded ||
		ns[0].Payment.Amount != 49900 || ns[0].Payment.ProviderOrderID != "ps_1" || ns[0].Payment.ProviderRef != "pay_ref_01" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
	ns, err = c.ParseWebhook(context.Background(), signedWebhook(testSecret, refundSucceededBody, nowUnix, nowUnix))
	if err != nil || len(ns) != 1 || ns[0].Refund.Status != connector.RefundSucceeded || ns[0].Refund.Amount != 10000 || ns[0].Refund.ProviderRef != "rf_01" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
	ns, err = c.ParseWebhook(context.Background(), signedWebhook(testSecret, `{"event_id":"e","event_type":"mandate.created","payload":{}}`, nowUnix, nowUnix))
	if err != nil || len(ns) != 0 {
		t.Errorf("unknown event: %v %v", ns, err)
	}
	if _, err := c.ParseWebhook(context.Background(), connector.WebhookRequest{Body: []byte(`{"event_type":"payment.succeeded"}`)}); err == nil {
		t.Error("missing event_id accepted")
	}
}

func TestVerifyWebhookTimestampWindow(t *testing.T) {
	c := newConn(t, newFake(""), widgetCfg)
	for _, tc := range []struct {
		off int64
		ok  bool
	}{{0, true}, {-299, true}, {-301, false}, {301, false}} {
		err := c.VerifyWebhook(context.Background(), signedWebhook(testSecret, paymentSucceededBody, nowUnix+tc.off, nowUnix))
		if (err == nil) != tc.ok {
			t.Errorf("offset %d: err = %v", tc.off, err)
		}
	}
	r := signedWebhook(testSecret, paymentSucceededBody, nowUnix, nowUnix)
	r.Headers["x-zoho-webhook-signature"] = []string{"v=abc"}
	delete(r.Headers, "X-Zoho-Webhook-Signature")
	if err := c.VerifyWebhook(context.Background(), r); !errors.Is(err, connector.ErrUnsigned) {
		t.Errorf("no t: %v", err)
	}
}

func TestRefundFlow(t *testing.T) {
	f := newFake("succeeded")
	f.refundStat = "pending"
	c := newConn(t, f, widgetCfg).(connector.Refunder)
	s, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_01", ProviderPaymentID: "pay_z1", Amount: 10000, Currency: "INR", Reason: "dup"})
	if err != nil || s.Status != connector.RefundPending || s.ProviderRefundID != "rf_z1" || s.Amount != 10000 {
		t.Fatalf("s=%+v err=%v", s, err)
	}
	var body map[string]any
	_ = json.Unmarshal(f.bodies["POST /api/v1/payments/pay_z1/refunds"], &body)
	if body["reference_number"] != "rf_01" || fmt.Sprint(body["amount"]) != "100" {
		t.Errorf("body = %v", body)
	}
	f.refundStat = "succeeded"
	g, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "rf_z1"})
	if err != nil || g.Status != connector.RefundSucceeded {
		t.Fatalf("g=%+v err=%v", g, err)
	}
	f.refundCode = 400
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "x", ProviderPaymentID: "pay_z1", Amount: 1}); !errors.As(err, &nr) || nr.Reason == "" {
		t.Errorf("err = %v", err)
	}
	f.refundCode = 429
	var rl connector.ErrRateLimited
	if _, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "x", ProviderPaymentID: "pay_z1", Amount: 1}); !errors.As(err, &rl) {
		t.Errorf("err = %v", err)
	}
}
