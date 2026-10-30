package wire

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
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
	s.SignRequest(h, "POST", "refund", body, now0)
	ver := func(s *Signer, h http.Header, body []byte, at time.Time) error {
		return s.VerifyRequest(h, "POST", "refund", body, at)
	}
	if err := ver(s, h, body, now0.Add(time.Minute)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	other, _ := NewSigner(SecretPrefix + base64.StdEncoding.EncodeToString([]byte("another secret entirely")))
	cases := map[string]func() error{
		"tampered body": func() error { return ver(s, h, []byte(`{"a":2}`), now0) },
		"wrong secret":  func() error { return ver(other, h, body, now0) },
		"stale":         func() error { return ver(s, h, body, now0.Add(Tolerance+time.Second)) },
		"future":        func() error { return ver(s, h, body, now0.Add(-Tolerance-time.Second)) },
		"no headers":    func() error { return ver(s, http.Header{}, body, now0) },
		"bad timestamp": func() error { g := h.Clone(); g.Set(HeaderTimestamp, "x"); return ver(s, g, body, now0) },
		"swapped id":    func() error { g := h.Clone(); g.Set(HeaderID, "msg_other"); return ver(s, g, body, now0) },
		"other method":  func() error { return s.VerifyRequest(h, "POST", "fetch_payment", body, now0) },
		"other verb":    func() error { return s.VerifyRequest(h, "GET", "refund", body, now0) },
		"a response": func() error {
			r := http.Header{}
			s.SignResponse(r, h.Get(HeaderID), body, now0)
			return ver(s, r, body, now0)
		},
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A rotated secret: the header may carry several signatures.
	g := h.Clone()
	g.Set(HeaderSignature, "v1,AAAA "+h.Get(HeaderSignature))
	if err := ver(s, g, body, now0); err != nil {
		t.Errorf("second signature: %v", err)
	}
	// Ids are fresh per message.
	h2 := http.Header{}
	s.SignRequest(h2, "POST", "refund", body, now0)
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
		"rejected":    {fmt.Errorf("razorpay: HTTP 400: bad plan: %w", connector.ErrRejected), 422, connector.ErrRejected},
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
		signer.SignRequest(req.Header, method, pathBase(path), []byte(body), now0)
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
	req := httptest.NewRequest("GET", "/prefix/v1/info", nil)
	reqID := signer.SignRequest(req.Header, "GET", MethodInfo, nil, now0)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var info connector.Info
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &info) != nil || info.Name != "stub" {
		t.Errorf("info under a prefix: %d %s", rec.Code, rec.Body)
	}
	if err := signer.VerifyResponse(rec.Header(), reqID, rec.Body.Bytes(), now0); err != nil {
		t.Errorf("response is not signed for its request: %v", err)
	}
	if err := signer.VerifyResponse(rec.Header(), "msg_another_request", rec.Body.Bytes(), now0); err == nil {
		t.Error("response verifies for a different request")
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

func pathBase(p string) string { return p[strings.LastIndex(p, "/")+1:] }

// A signed request is for one method: posting it to another path, even one
// whose handler would decode the same body, is refused before any dispatch.
func TestWireRejectsMethodSwap(t *testing.T) {
	f := &stub{}
	h, _ := NewHandler(f, secret, func() time.Time { return now0 })
	signer, _ := NewSigner(secret)

	body := `{"provider_ref":"R1"}`
	req := httptest.NewRequest("POST", "/fetch_payment", strings.NewReader(body))
	signer.SignRequest(req.Header, "POST", MethodFetchPayment, []byte(body), now0)

	swapped := req.Clone(req.Context())
	swapped.URL.Path = "/refund"
	swapped.Body = io.NopCloser(strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, swapped)
	if rec.Code != 401 {
		t.Fatalf("a request signed for fetch_payment was served at /refund: %d %s", rec.Code, rec.Body)
	}
	// Another method that would also decode the body is refused the same way.
	for _, m := range []string{"create_checkout", "handle_return", "charge_mandate", "revoke_mandate"} {
		again := httptest.NewRequest("POST", "/"+m, strings.NewReader(body))
		again.Header = req.Header.Clone()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, again)
		if rec.Code != 401 {
			t.Errorf("%s: %d", m, rec.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a swapped request reached the connector: %v", f.calls)
	}
	// The genuine request still works.
	ok := httptest.NewRequest("POST", "/fetch_payment", strings.NewReader(body))
	ok.Header = req.Header.Clone()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, ok)
	if rec.Code == 401 {
		t.Errorf("the genuine request was refused: %s", rec.Body)
	}
}

// A captured signed request is served once: sending the same bytes again,
// even inside the timestamp tolerance, is refused and never reaches the connector.
func TestWireRejectsReplay(t *testing.T) {
	now := now0
	f := &stub{}
	h, _ := NewHandler(f, secret, func() time.Time { return now })
	signer, _ := NewSigner(secret)
	body := `{"provider_ref":"R1"}`
	send := func(hdr http.Header) int {
		req := httptest.NewRequest("POST", "/create_checkout", strings.NewReader(body))
		req.Header = hdr.Clone()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	hdr := http.Header{}
	signer.SignRequest(hdr, "POST", MethodCreateCheckout, []byte(body), now)
	if code := send(hdr); code != 200 {
		t.Fatalf("first delivery: %d", code)
	}
	for i := 0; i < 3; i++ {
		now = now.Add(time.Minute)
		if code := send(hdr); code != 401 {
			t.Fatalf("replay %d accepted: %d", i, code)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("connector saw %d calls", len(f.calls))
	}
	// A fresh message is not affected, and a replayed id is refused after the
	// timestamp tolerance too (by the signature check, once the cache forgets it).
	fresh := http.Header{}
	signer.SignRequest(fresh, "POST", MethodCreateCheckout, []byte(body), now)
	if code := send(fresh); code != 200 {
		t.Errorf("fresh request: %d", code)
	}
	now = now.Add(3 * Tolerance)
	if code := send(hdr); code != 401 {
		t.Errorf("old request after the window: %d", code)
	}
}

// The cache never forgets an id that is still inside its window: when it is
// full it refuses new ids (the caller answers 503) instead of evicting the
// oldest, so a replay inside the window is always refused. Expired ids free
// their place.
func TestReplayCacheNeverEvictsAnIdInsideTheWindow(t *testing.T) {
	c := newReplayCache(3, time.Minute)
	for _, id := range []string{"a", "b", "c"} {
		if fresh, full := c.firstSeen(id, now0); !fresh || full {
			t.Fatalf("%s: fresh=%v full=%v", id, fresh, full)
		}
	}
	if fresh, full := c.firstSeen("d", now0.Add(30*time.Second)); fresh || !full {
		t.Fatalf("a full cache must refuse a new id, not evict: fresh=%v full=%v", fresh, full)
	}
	for _, id := range []string{"a", "b", "c"} {
		if fresh, full := c.firstSeen(id, now0.Add(59*time.Second)); fresh || full {
			t.Errorf("%s was forgotten inside its window: fresh=%v full=%v", id, fresh, full)
		}
	}
	if c.order.Len() != 3 || len(c.ids) != 3 {
		t.Errorf("cache changed size: %d %d", c.order.Len(), len(c.ids))
	}
	if fresh, full := c.firstSeen("d", now0.Add(2*time.Minute)); !fresh || full || c.order.Len() != 1 {
		t.Errorf("expired ids are not forgotten: fresh=%v full=%v len=%d", fresh, full, c.order.Len())
	}
}

// At the wire: with a full cache a new request gets 503 (temporary, so the
// caller retries with a new id) and every request that was served stays
// refused as a replay for the whole window, however many others arrive.
func TestWireFullReplayCacheAnswers503AndStillRefusesReplays(t *testing.T) {
	now := now0
	f := &stub{}
	h, _ := NewHandler(f, secret, func() time.Time { return now }, WithReplayCacheSize(4))
	signer, _ := NewSigner(secret)
	send := func(hdr http.Header) int {
		req := httptest.NewRequest("POST", "/create_checkout", strings.NewReader(`{"provider_ref":"R"}`))
		req.Header = hdr.Clone()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	sign := func() http.Header {
		hdr := http.Header{}
		signer.SignRequest(hdr, "POST", MethodCreateCheckout, []byte(`{"provider_ref":"R"}`), now)
		return hdr
	}
	var served []http.Header
	for i := 0; i < 4; i++ {
		hdr := sign()
		if code := send(hdr); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
		served = append(served, hdr)
		now = now.Add(time.Second)
	}
	if code := send(sign()); code != 503 {
		t.Fatalf("a full cache answered %d, want 503", code)
	}
	for round := 0; round < 3; round++ {
		now = now.Add(time.Minute)
		for i, hdr := range served {
			if code := send(hdr); code != 401 {
				t.Fatalf("round %d: replay of request %d answered %d", round, i, code)
			}
		}
	}
	if len(f.calls) != 4 {
		t.Errorf("connector saw %d calls, want 4", len(f.calls))
	}
	// Once the window has passed the places are free again.
	now = now.Add(2*Tolerance + time.Minute)
	if code := send(sign()); code != 200 {
		t.Errorf("after the window: %d", code)
	}
}
