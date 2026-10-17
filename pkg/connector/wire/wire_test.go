package wire

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

var (
	secret = SecretPrefix + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	now0   = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
)

func TestSignVerify(t *testing.T) {
	s, err := NewSigner(secret)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"a":1}`)
	h := http.Header{}
	s.Sign(h, body, now0)
	if err := s.Verify(h, body, now0.Add(time.Minute)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	other, _ := NewSigner(SecretPrefix + base64.StdEncoding.EncodeToString([]byte("another secret entirely")))
	cases := map[string]func() error{
		"tampered body": func() error { return s.Verify(h, []byte(`{"a":2}`), now0) },
		"wrong secret":  func() error { return other.Verify(h, body, now0) },
		"stale":         func() error { return s.Verify(h, body, now0.Add(Tolerance+time.Second)) },
		"future":        func() error { return s.Verify(h, body, now0.Add(-Tolerance-time.Second)) },
		"no headers":    func() error { return s.Verify(http.Header{}, body, now0) },
		"bad timestamp": func() error { g := h.Clone(); g.Set(HeaderTimestamp, "x"); return s.Verify(g, body, now0) },
		"swapped id":    func() error { g := h.Clone(); g.Set(HeaderID, "msg_other"); return s.Verify(g, body, now0) },
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A rotated secret: the header may carry several signatures.
	g := h.Clone()
	g.Set(HeaderSignature, "v1,AAAA "+h.Get(HeaderSignature))
	if err := s.Verify(g, body, now0); err != nil {
		t.Errorf("second signature: %v", err)
	}
	// Ids are fresh per message.
	h2 := http.Header{}
	s.Sign(h2, body, now0)
	if h2.Get(HeaderID) == h.Get(HeaderID) {
		t.Error("message id reused")
	}
	for _, bad := range []string{"", "whsec_", "whsec_!!!", "not base64!"} {
		if _, err := NewSigner(bad); err == nil {
			t.Errorf("accepted secret %q", bad)
		}
	}
}

func TestErrorsRoundTripThroughTheWire(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		is     error
	}{
		"not found":   {connector.ErrNotFound, 422, connector.ErrNotFound},
		"credentials": {fmt.Errorf("x: %w", connector.ErrInvalidCredentials), 422, connector.ErrInvalidCredentials},
		"temporary":   {connector.ErrTemporary, 503, connector.ErrTemporary},
		"unsupported": {connector.ErrUnsupported, 422, connector.ErrUnsupported},
		"signature":   {connector.ErrInvalidSignature, 422, connector.ErrInvalidSignature},
		"unsigned":    {connector.ErrUnsigned, 422, connector.ErrUnsigned},
		"return":      {connector.ErrInvalidReturn, 422, connector.ErrInvalidReturn},
		"limit":       {connector.ErrMandateLimit, 422, connector.ErrMandateLimit},
		"internal":    {errors.New("boom"), 500, connector.ErrTemporary},
	} {
		status, e := FromError(tc.err)
		raw, _ := json.Marshal(Envelope{e})
		var back Envelope
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if status != tc.status || !errors.Is(back.Error.Err(), tc.is) {
			t.Errorf("%s: status %d, back %v", name, status, back.Error.Err())
		}
	}
	status, e := FromError(connector.ErrNotRefundable{Reason: "too old"})
	var nr connector.ErrNotRefundable
	if status != 422 || !errors.As(e.Err(), &nr) || nr.Reason != "too old" {
		t.Errorf("not refundable: %d %v", status, e.Err())
	}
	status, e = FromError(connector.ErrRateLimited{RetryAfter: 1500 * time.Millisecond})
	var rl connector.ErrRateLimited
	if status != 429 || !errors.As(e.Err(), &rl) || rl.RetryAfter != 1500*time.Millisecond {
		t.Errorf("rate limited: %d %v", status, e.Err())
	}
	if err := (Error{Code: "from_the_future", Message: "m"}).Err(); !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("unknown code: %v", err)
	}
	if err := (Error{Code: CodeBadRequest, Message: "m"}).Err(); errors.Is(err, connector.ErrTemporary) {
		t.Errorf("bad request must not be retryable: %v", err)
	}
}

// stub is a connector with no optional interfaces.
type stub struct{ calls []string }

func (s *stub) Info() connector.Info { return connector.Info{Name: "stub", DisplayName: "Stub"} }
func (s *stub) CreateCheckout(_ context.Context, r connector.CheckoutRequest) (connector.CheckoutResult, error) {
	s.calls = append(s.calls, "create_checkout")
	return connector.CheckoutResult{ProviderOrderID: "order_" + r.ProviderRef}, nil
}
func (s *stub) HandleReturn(context.Context, connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{}, connector.ErrInvalidReturn
}
func (s *stub) VerifyWebhook(context.Context, connector.WebhookRequest) error {
	return connector.ErrInvalidSignature
}
func (s *stub) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	return nil, nil
}
func (s *stub) FetchPayment(context.Context, connector.PaymentRef) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{}, errors.New("boom")
}

func post(t *testing.T, h http.Handler, signer *Signer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if signer != nil {
		signer.Sign(req.Header, []byte(body), now0)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler(t *testing.T) {
	h, err := NewHandler(&stub{}, secret, func() time.Time { return now0 })
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := NewSigner(secret)

	rec := post(t, h, nil, "POST", "/create_checkout", `{}`)
	if rec.Code != 401 || rec.Header().Get(HeaderSignature) == "" {
		t.Errorf("unsigned request: %d %v", rec.Code, rec.Header())
	}
	rec = post(t, h, signer, "GET", "/prefix/v1/info", ``)
	var info connector.Info
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.Name != "stub" {
		t.Errorf("info under a prefix: %d %s", rec.Code, rec.Body)
	}
	if err := signer.Verify(rec.Header(), rec.Body.Bytes(), now0); err != nil {
		t.Errorf("response is not signed: %v", err)
	}
	rec = post(t, h, signer, "POST", "/create_checkout", `{"provider_ref":"R1"}`)
	var res connector.CheckoutResult
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil || res.ProviderOrderID != "order_R1" {
		t.Errorf("create_checkout: %d %s", rec.Code, rec.Body)
	}

	errCode := func(method, path, body string) (int, string) {
		rec := post(t, h, signer, method, path, body)
		var env Envelope
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		return rec.Code, env.Error.Code
	}
	for name, tc := range map[string]struct {
		method, path, body string
		status             int
		code               string
	}{
		"typed error":      {"POST", "/handle_return", `{}`, 422, CodeInvalidReturn},
		"internal":         {"POST", "/fetch_payment", `{}`, 500, CodeInternal},
		"not implemented":  {"POST", "/refund", `{}`, 422, CodeUnsupported},
		"bad json":         {"POST", "/create_checkout", `{`, 400, CodeBadRequest},
		"unknown method":   {"POST", "/frobnicate", `{}`, 400, CodeBadRequest},
		"wrong verb":       {"GET", "/create_checkout", ``, 405, CodeBadRequest},
		"optional missing": {"POST", "/charge_mandate", `{}`, 422, CodeUnsupported},
	} {
		if status, code := errCode(tc.method, tc.path, tc.body); status != tc.status || code != tc.code {
			t.Errorf("%s: %d %s", name, status, code)
		}
	}
	if _, err := NewHandler(&stub{}, "not base64!", nil); err == nil {
		t.Error("accepted a bad secret")
	}
}
