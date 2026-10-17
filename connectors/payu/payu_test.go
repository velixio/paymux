package payu

import (
	"context"
	"encoding/json"
	"errors"
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
	testKey  = "testkey1"
	testSalt = "testsalt1"

	// Computed independently with Python hashlib (see the spec formulas).
	goldenRequestHash  = "63ce9a307c228bece20f39791a79850658939161dccb240f6af908829480fdf04e66305e8bfac0ffe3062ce1895b416645b994d18f038180d55e67e7f8111660"
	goldenReverseHash  = "ab007af5c7a2859efe25203c1443edc5073696b9b57a8e2f1ce3d38cbb12862c9f73c9d8b0347d35717ab0ca335c0972ffd58a6b8e5cfc81f9ee5b2068dfe6c7"
	goldenReverseAddl  = "e38580855e6aa65604ac67d9c953c6e6827d2165b7bf98b8a29fefad573ae9492417173c0551eacd68600c5c87c7b7d65eb6426471e92a75bdc3bac01e271566"
	goldenCommandHash  = "f757cb0bd8c77b8a0520eb6c341eb481dd9afeec941cbf59edcb58d7649377ca69e9ff24255c7fd2453ffea0aeb8e8c0922537af3cf5c87903914ac23dfa1c7d"
	testConfigJSON     = `{"key":"testkey1","salt":"testsalt1","test_mode":true}`
	wrongConfigJSON    = `{"key":"testkey1","salt":"other-salt","test_mode":true}`
	paymentFormSuccess = "success"
)

var testConfig = json.RawMessage(testConfigJSON)

func TestGoldenHashes(t *testing.T) {
	if got := requestHash(testKey, testSalt, "pay_ref_01", "499.00", "Pro plan", "Asha", "asha@example.com", [5]string{"att_01"}); got != goldenRequestHash {
		t.Errorf("request hash = %s", got)
	}
	f := successForm(testSalt, "")
	f.Del("hash")
	if got := reverseHash(testSalt, f); got != goldenReverseHash {
		t.Errorf("reverse hash = %s", got)
	}
	f.Set("additionalCharges", "12.50")
	if got := reverseHash(testSalt, f); got != goldenReverseAddl {
		t.Errorf("reverse hash with additionalCharges = %s", got)
	}
	if got := sha512Hex(testKey, "verify_payment", "pay_ref_01", testSalt); got != goldenCommandHash {
		t.Errorf("command hash = %s", got)
	}
}

// successForm builds a PayU success response/webhook signed with salt.
func successForm(salt, extra string) url.Values {
	f := url.Values{
		"key": {testKey}, "txnid": {"pay_ref_01"}, "mihpayid": {"403993715521"}, "status": {paymentFormSuccess},
		"unmappedstatus": {"captured"}, "amount": {"499.00"}, "productinfo": {"Pro plan"}, "firstname": {"Asha"},
		"email": {"asha@example.com"}, "udf1": {"att_01"}, "mode": {"UPI"}, "addedon": {"2026-10-17 17:30:00"},
	}
	if extra != "" {
		f.Set("additionalCharges", extra)
	}
	f.Set("hash", reverseHash(salt, f))
	return f
}

func webhook(f url.Values) connector.WebhookRequest {
	return connector.WebhookRequest{
		Method: "POST", Body: []byte(keyLast(f)), ReceivedAt: 1,
		Headers: map[string][]string{"Content-Type": {"application/x-www-form-urlencoded"}},
	}
}

// keyLast encodes the form with "key" as the last field, so an appended byte
// always lands in a hashed value (PayU only hashes a subset of the fields).
func keyLast(f url.Values) string {
	g := url.Values{}
	for k, v := range f {
		if k != "key" {
			g[k] = v
		}
	}
	return g.Encode() + "&key=" + url.QueryEscape(f.Get("key"))
}

// fake is a PayU postservice.
type fake struct {
	mu      sync.Mutex
	forms   []url.Values
	verify  string // JSON body for verify_payment
	refund  string
	check   string
	listing string
	cmds    map[string]string // other commands (standing instructions)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.forms = append(f.forms, r.PostForm)
	f.mu.Unlock()
	if r.URL.Path != "/merchant/postservice.php" || r.URL.Query().Get("form") != "2" {
		http.NotFound(w, r)
		return
	}
	// Independent re-computation of sha512(key|command|var1|salt).
	if r.PostForm.Get("hash") != sha512Hex(testKey, r.PostForm.Get("command"), r.PostForm.Get("var1"), testSalt) {
		_, _ = w.Write([]byte(`{"status":0,"msg":"Invalid hash"}`))
		return
	}
	body := map[string]string{"verify_payment": f.verify, "cancel_refund_transaction": f.refund, "check_action_status": f.check, "get_Transaction_Details": f.listing}[r.PostForm.Get("command")]
	if b, ok := f.cmds[r.PostForm.Get("command")]; ok {
		body = b
	}
	_, _ = w.Write([]byte(body))
}

func (f *fake) last() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forms[len(f.forms)-1]
}

func verifyBody(status, unmapped string) string {
	return `{"status":1,"msg":"1 out of 1 Transactions Fetched Successfully","transaction_details":{"pay_ref_01":{"mihpayid":"403993715521","txnid":"pay_ref_01","status":"` + status +
		`","unmappedstatus":"` + unmapped + `","amt":"499.00","mode":"UPI","error_code":"E000","error_Message":"No Error","addedon":"2026-10-17 17:30:00"}}}`
}

func newConn(t *testing.T, f *fake) connector.Connector {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(1760000000, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRegistered(t *testing.T) {
	if _, info, ok := connector.Lookup("payu"); !ok || info.DisplayName == "" {
		t.Fatal("payu not registered")
	}
}

func TestConfigValidation(t *testing.T) {
	d := connectortest.NewDeps(nil, time.Now())
	for _, cfg := range []string{`{}`, `{"key":"a"}`, `x`} {
		if _, err := New(json.RawMessage(cfg), d); err == nil {
			t.Errorf("config %s accepted", cfg)
		}
	}
}

func TestCreateCheckoutFormPost(t *testing.T) {
	c := newConn(t, &fake{})
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{
		AttemptID: "att_01", ProviderRef: "pay_ref_01", Amount: 49900, Currency: "INR", Description: "Pro plan",
		Customer: connector.Customer{Name: "Asha", Email: "asha@example.com", Phone: "9999999999"},
	})
	if err != nil {
		t.Fatal(err)
	}
	na := res.NextAction
	if na.Kind != connector.ActionFormPost || na.URL != "https://test.payu.in/_payment" {
		t.Fatalf("na = %+v", na)
	}
	fl := na.Fields
	if fl["txnid"] != "pay_ref_01" || fl["amount"] != "499.00" || fl["udf1"] != "att_01" || fl["key"] != testKey ||
		fl["surl"] != "https://pay.example.com/return/acc/pay_ref_01" || fl["furl"] != fl["surl"] || fl["hash"] != goldenRequestHash {
		t.Errorf("fields = %+v", fl)
	}
	if _, leaked := fl["salt"]; leaked {
		t.Error("salt in form")
	}
}

func TestCreateCheckoutProdURLAndValidation(t *testing.T) {
	c, _ := New(json.RawMessage(`{"key":"k","salt":"s"}`), connectortest.NewDeps(nil, time.Now()))
	res, err := c.CreateCheckout(context.Background(), connector.CheckoutRequest{ProviderRef: "r", Amount: 100, Currency: "INR"})
	if err != nil || res.NextAction.URL != "https://secure.payu.in/_payment" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.NextAction.Fields["firstname"] == "" || res.NextAction.Fields["email"] == "" || res.NextAction.Fields["productinfo"] == "" {
		t.Error("placeholders missing")
	}
	for _, r := range []connector.CheckoutRequest{
		{ProviderRef: "r", Amount: 100, Currency: "USD"},
		{ProviderRef: "", Amount: 100, Currency: "INR"},
		{ProviderRef: "r", Amount: 0, Currency: "INR"},
	} {
		if _, err := c.CreateCheckout(context.Background(), r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestHandleReturn(t *testing.T) {
	c := newConn(t, &fake{})
	ok := successForm(testSalt, "")
	got, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "pay_ref_01", Form: ok})
	if err != nil || got.ProviderPaymentID != "403993715521" || got.ProviderRef != "pay_ref_01" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	withAddl := successForm(testSalt, "12.50")
	if _, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "pay_ref_01", Form: withAddl}); err != nil {
		t.Errorf("additionalCharges return rejected: %v", err)
	}
	tampered := successForm(testSalt, "")
	tampered.Set("amount", "1.00")
	forged := successForm("other-salt", "")
	wrongRef := successForm(testSalt, "")
	for name, rq := range map[string]connector.ReturnRequest{
		"tampered amount": {ProviderRef: "pay_ref_01", Form: tampered},
		"wrong salt":      {ProviderRef: "pay_ref_01", Form: forged},
		"other attempt":   {ProviderRef: "pay_ref_99", Form: wrongRef},
		"no hash":         {ProviderRef: "pay_ref_01", Form: url.Values{"txnid": {"pay_ref_01"}, "status": {"success"}}},
	} {
		if _, err := c.HandleReturn(context.Background(), rq); !errors.Is(err, connector.ErrInvalidReturn) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A body-only return (no parsed form) works too.
	if _, err := c.HandleReturn(context.Background(), connector.ReturnRequest{ProviderRef: "pay_ref_01", Body: []byte(ok.Encode())}); err != nil {
		t.Errorf("body return: %v", err)
	}
}

func TestVerifyWebhookRefundIsUnsigned(t *testing.T) {
	c := newConn(t, &fake{})
	refund := url.Values{"mihpayid": {"403993715521"}, "requestId": {"rq1"}, "status": {"success"}, "amount": {"100.00"}, "action": {"refund"}}
	if err := c.VerifyWebhook(context.Background(), webhook(refund)); !errors.Is(err, connector.ErrUnsigned) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseWebhook(t *testing.T) {
	c := newConn(t, &fake{})
	for raw, want := range map[string]connector.PaymentStatus{
		"success": connector.PaymentSucceeded, "failure": connector.PaymentFailed,
		"pending": connector.PaymentProcessing, "userCancelled": connector.PaymentCanceled,
	} {
		f := successForm(testSalt, "")
		f.Set("status", raw)
		ns, err := c.ParseWebhook(context.Background(), webhook(f))
		if err != nil || len(ns) != 1 {
			t.Fatalf("%s: %v %v", raw, ns, err)
		}
		p := ns[0].Payment
		if p.Status != want || p.RawStatus != raw || p.Amount != 49900 || p.ProviderRef != "pay_ref_01" || p.ProviderPaymentID != "403993715521" {
			t.Errorf("%s: %+v", raw, p)
		}
		// 2026-10-17 17:30 IST == 12:00 UTC
		if p.CreatedAt != time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC).UnixMilli() {
			t.Errorf("createdAt = %d", p.CreatedAt)
		}
	}
	a, _ := c.ParseWebhook(context.Background(), webhook(successForm(testSalt, "")))
	b, _ := c.ParseWebhook(context.Background(), webhook(successForm(testSalt, "")))
	if a[0].EventID != b[0].EventID {
		t.Error("event id not stable across retries")
	}
	// Non-payment form (refund notice) yields nothing.
	ns, err := c.ParseWebhook(context.Background(), webhook(url.Values{"requestId": {"x"}}))
	if err != nil || len(ns) != 0 {
		t.Errorf("ns=%v err=%v", ns, err)
	}
}

func TestFetchPaymentSendsHash(t *testing.T) {
	f := &fake{verify: verifyBody("success", "captured")}
	c := newConn(t, f)
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"})
	if err != nil || snap.Status != connector.PaymentSucceeded {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if got := f.last(); got.Get("hash") != goldenCommandHash || got.Get("command") != "verify_payment" || got.Get("var1") != "pay_ref_01" || got.Get("key") != testKey {
		t.Errorf("form = %v", got)
	}
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderPaymentID: "x"}); err == nil {
		t.Error("payment-id-only ref accepted")
	}
}

func TestFetchPaymentNotFoundAndBadCredentials(t *testing.T) {
	f := &fake{verify: `{"status":0,"msg":"0 out of 1 Transactions Fetched Successfully","transaction_details":{}}`}
	c := newConn(t, f)
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	bad, _ := New(json.RawMessage(wrongConfigJSON), connectortest.NewDeps(srv, time.Now()))
	if _, err := bad.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "pay_ref_01"}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
}

func TestRefund(t *testing.T) {
	f := &fake{refund: `{"status":1,"msg":"Refund Request Queued","request_id":"7800009","bank_ref_num":null,"mihpayid":403993715521}`}
	c := newConn(t, f).(connector.Refunder)
	snap, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "rf_0123456789", ProviderPaymentID: "403993715521", Amount: 10000, Currency: "INR"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != connector.RefundPending || snap.ProviderRefundID != "7800009" || snap.ProviderRef != "rf_0123456789" || snap.Amount != 10000 {
		t.Errorf("snap = %+v", snap)
	}
	got := f.last()
	if got.Get("command") != "cancel_refund_transaction" || got.Get("var1") != "403993715521" || got.Get("var2") != "rf_0123456789" || got.Get("var3") != "100.00" {
		t.Errorf("form = %v", got)
	}
	if _, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: strings.Repeat("x", 24), ProviderPaymentID: "1", Amount: 1}); err == nil {
		t.Error("24-char token accepted")
	}
	f.refund = `{"status":0,"msg":"Refund amount exceeds"}`
	var nr connector.ErrNotRefundable
	if _, err := c.Refund(context.Background(), connector.RefundRequest{ProviderRef: "r", ProviderPaymentID: "1", Amount: 1}); !errors.As(err, &nr) || nr.Reason == "" {
		t.Errorf("err = %v", err)
	}
}

func TestFetchRefund(t *testing.T) {
	f := &fake{check: `{"status":1,"msg":"ok","transaction_details":{"403993715521":{"7800009":{"mihpayid":"403993715521","request_id":"7800009","status":"success","amt":"100.00"}}}}`}
	c := newConn(t, f).(connector.Refunder)
	s, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "7800009", ProviderRef: "rf_1"})
	if err != nil || s.Status != connector.RefundSucceeded || s.Amount != 10000 || s.ProviderPaymentID != "403993715521" {
		t.Fatalf("s=%+v err=%v", s, err)
	}
	f.check = `{"status":0,"msg":"nothing","transaction_details":{}}`
	if _, err := c.FetchRefund(context.Background(), connector.RefundRef{ProviderRefundID: "x"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestListPayments(t *testing.T) {
	f := &fake{listing: `{"status":1,"msg":"ok","Transaction_details":[
	  {"mihpayid":"1","txnid":"a","status":"success","amt":"10.00","addedon":"2026-10-17 17:30:00"},
	  {"mihpayid":"2","txnid":"b","status":"failure","amt":"20.00","addedon":"2026-10-10 01:00:00"}]}`}
	c := newConn(t, f).(connector.Lister)
	from := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC).UnixMilli()
	pg, err := c.ListPayments(context.Background(), connector.TimeRange{From: from, To: from + 86400000}, "")
	if err != nil || len(pg.Items) != 1 || pg.Items[0].ProviderRef != "a" || pg.Items[0].Amount != 1000 {
		t.Fatalf("pg=%+v err=%v", pg, err)
	}
	if got := f.last(); got.Get("command") != "get_Transaction_Details" || got.Get("var1") != "2026-10-17" || got.Get("var2") != "2026-10-18" {
		t.Errorf("form = %v", got)
	}
	if _, err := c.ListRefunds(context.Background(), connector.TimeRange{}, ""); !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("ListRefunds err = %v", err)
	}
}

func TestStatusMapTotal(t *testing.T) {
	for raw, want := range map[string]connector.PaymentStatus{
		"success": connector.PaymentSucceeded, "failure": connector.PaymentFailed, "pending": connector.PaymentProcessing,
		"userCancelled": connector.PaymentCanceled, "Not Found": connector.PaymentRequiresAction, "weird": connector.PaymentProcessing,
	} {
		if mapStatus(raw) != want {
			t.Errorf("%s -> %s", raw, mapStatus(raw))
		}
	}
}
