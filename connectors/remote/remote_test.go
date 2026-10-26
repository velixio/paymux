package remote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
	"github.com/paymux/paymux/pkg/connector/wire"
)

var (
	testSecret = wire.SecretPrefix + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	now0       = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
)

// fullFake implements every optional interface and records what reached it.
type fullFake struct {
	mu    sync.Mutex
	calls []string
	args  map[string]any
	caps  []connector.Capability
	err   error // returned by every call when set
}

func (f *fullFake) rec(name string, arg any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.args == nil {
		f.args = map[string]any{}
	}
	f.args[name] = arg
	return f.err
}

func (f *fullFake) Info() connector.Info {
	return connector.Info{Name: "upstream", DisplayName: "Upstream", Capabilities: f.caps, MerchantOfRecord: true, Currencies: []string{"USD"}}
}
func (f *fullFake) CreateCheckout(_ context.Context, r connector.CheckoutRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{ProviderOrderID: "o1", NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: "https://x.example.com"}}, f.rec("create_checkout", r)
}
func (f *fullFake) HandleReturn(_ context.Context, r connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{ProviderRef: r.ProviderRef}, f.rec("handle_return", r)
}
func (f *fullFake) VerifyWebhook(_ context.Context, r connector.WebhookRequest) error {
	return f.rec("verify_webhook", r)
}
func (f *fullFake) ParseWebhook(_ context.Context, r connector.WebhookRequest) ([]connector.Notification, error) {
	return []connector.Notification{{Kind: connector.NotifyPayment, EventID: "e", EventType: "t", Payment: &connector.PaymentSnapshot{ProviderRef: "r"}}}, f.rec("parse_webhook", r)
}
func (f *fullFake) FetchPayment(_ context.Context, r connector.PaymentRef) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{ProviderRef: r.ProviderRef, Amount: 5}, f.rec("fetch_payment", r)
}
func (f *fullFake) Refund(_ context.Context, r connector.RefundRequest) (connector.RefundSnapshot, error) {
	return connector.RefundSnapshot{ProviderRefundID: "rf1"}, f.rec("refund", r)
}
func (f *fullFake) FetchRefund(_ context.Context, r connector.RefundRef) (connector.RefundSnapshot, error) {
	return connector.RefundSnapshot{ProviderRefundID: r.ProviderRefundID}, f.rec("fetch_refund", r)
}
func (f *fullFake) ListDisputes(_ context.Context, tr connector.TimeRange, c connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	return connector.Page[connector.DisputeSnapshot]{Items: []connector.DisputeSnapshot{{ProviderDisputeID: "d1"}}, Next: "n"}, f.rec("list_disputes", []any{tr, c})
}
func (f *fullFake) FetchDispute(_ context.Context, r connector.DisputeRef) (connector.DisputeSnapshot, error) {
	return connector.DisputeSnapshot{ProviderDisputeID: r.ProviderDisputeID}, f.rec("fetch_dispute", r)
}
func (f *fullFake) AcceptDispute(_ context.Context, r connector.DisputeRef) error {
	return f.rec("accept_dispute", r)
}
func (f *fullFake) SubmitEvidence(_ context.Context, r connector.DisputeRef, e connector.Evidence) error {
	return f.rec("submit_evidence", []any{r, e})
}
func (f *fullFake) ListPayments(_ context.Context, tr connector.TimeRange, c connector.Cursor) (connector.Page[connector.PaymentSnapshot], error) {
	return connector.Page[connector.PaymentSnapshot]{Items: []connector.PaymentSnapshot{{ProviderRef: "p1"}}}, f.rec("list_payments", []any{tr, c})
}
func (f *fullFake) ListRefunds(_ context.Context, tr connector.TimeRange, c connector.Cursor) (connector.Page[connector.RefundSnapshot], error) {
	return connector.Page[connector.RefundSnapshot]{Items: []connector.RefundSnapshot{{ProviderRefundID: "r1"}}}, f.rec("list_refunds", []any{tr, c})
}
func (f *fullFake) ListSettlements(_ context.Context, tr connector.TimeRange) ([]connector.SettlementSnapshot, error) {
	return []connector.SettlementSnapshot{{ProviderSettlementID: "s1", Amount: 9}}, f.rec("list_settlements", tr)
}
func (f *fullFake) CancelCheckout(_ context.Context, r connector.PaymentRef) error {
	return f.rec("cancel_checkout", r)
}
func (f *fullFake) CreateSubscriptionCheckout(_ context.Context, r connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{ProviderOrderID: "sub_o"}, f.rec("create_subscription_checkout", r)
}
func (f *fullFake) FetchSubscription(_ context.Context, r connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	return connector.SubscriptionSnapshot{ProviderSubscriptionID: r.ProviderSubscriptionID}, f.rec("fetch_subscription", r)
}
func (f *fullFake) CancelSubscription(_ context.Context, r connector.SubscriptionRef, end bool) error {
	return f.rec("cancel_subscription", []any{r, end})
}
func (f *fullFake) UpdateSubscriptionAmount(_ context.Context, r connector.SubscriptionRef, amount int64) error {
	return f.rec("update_subscription_amount", []any{r, amount})
}
func (f *fullFake) CreateMandateCheckout(_ context.Context, r connector.MandateRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{ProviderOrderID: "m_o"}, f.rec("create_mandate_checkout", r)
}
func (f *fullFake) FetchMandate(_ context.Context, r connector.MandateRef) (connector.MandateSnapshot, error) {
	return connector.MandateSnapshot{ProviderMandateID: r.ProviderMandateID, Status: "active"}, f.rec("fetch_mandate", r)
}
func (f *fullFake) NotifyPreDebit(_ context.Context, r connector.PreDebitRequest) error {
	return f.rec("notify_pre_debit", r)
}
func (f *fullFake) ChargeMandate(_ context.Context, r connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	return connector.PaymentSnapshot{ProviderRef: r.ProviderRef}, f.rec("charge_mandate", r)
}
func (f *fullFake) RevokeMandate(_ context.Context, r connector.MandateRef) error {
	return f.rec("revoke_mandate", r)
}
func (f *fullFake) CreatePayout(_ context.Context, r connector.PayoutRequest) (connector.PayoutSnapshot, error) {
	return connector.PayoutSnapshot{ProviderRef: r.ProviderRef, ProviderPayoutID: "po_1", Status: connector.PayoutProcessing}, f.rec("create_payout", r)
}
func (f *fullFake) FetchPayout(_ context.Context, r connector.PayoutRef) (connector.PayoutSnapshot, error) {
	return connector.PayoutSnapshot{ProviderPayoutID: r.ProviderPayoutID, Status: connector.PayoutSucceeded}, f.rec("fetch_payout", r)
}

// bare implements only the required interface.
type bare struct{ *fullFake }

func (b bare) Info() connector.Info { return connector.Info{Name: "bare"} }

var allCaps = []connector.Capability{connector.CapRefund, connector.CapDisputeRead, connector.CapDisputeRespond, connector.CapList,
	connector.CapSettle, connector.CapCancel, connector.CapSubscriptionManager, connector.CapMandateManager, connector.CapPayout}

// serve exposes c at a test server and returns a remote connector for it.
func serve(t *testing.T, c connector.Connector, wrap func(http.Handler) http.Handler) connector.Connector {
	t.Helper()
	h, err := wire.NewHandler(c, testSecret, func() time.Time { return now0 })
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return dial(t, srv, testSecret)
}

func dial(t *testing.T, srv *httptest.Server, secret string) connector.Connector {
	t.Helper()
	cfg, _ := json.Marshal(config{BaseURL: srv.URL + "/v1/", Secret: secret, AllowInsecure: true, CheckoutHosts: testHosts}) // httptest listens on loopback
	deps := connectortest.NewDeps(nil, now0)
	deps.HTTP = srv.Client()
	rc, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func TestDiscoveryDecidesTheOptionalInterfaces(t *testing.T) {
	has := func(c connector.Connector) (m [9]bool) {
		_, m[0] = c.(connector.Refunder)
		_, m[1] = c.(connector.DisputeReader)
		_, m[2] = c.(connector.DisputeResponder)
		_, m[3] = c.(connector.Lister)
		_, m[4] = c.(connector.Settler)
		_, m[5] = c.(connector.Canceler)
		_, m[6] = c.(connector.SubscriptionManager)
		_, m[7] = c.(connector.MandateManager)
		_, m[8] = c.(connector.Payouter)
		return
	}
	// Every one of the 512 capability sets yields exactly those interfaces.
	for mask := 0; mask < 512; mask++ {
		var caps []connector.Capability
		var want [9]bool
		for i, c := range allCaps {
			if mask&(1<<i) != 0 {
				caps = append(caps, c)
				want[i] = true
			}
		}
		rc := serve(t, &fullFake{caps: caps}, nil)
		if got := has(rc); got != want {
			t.Fatalf("mask %09b: interfaces %v want %v", mask, got, want)
		}
		if info := rc.Info(); info.Name != "remote" || info.DisplayName != "Upstream" || !info.MerchantOfRecord || !reflect.DeepEqual(info.Capabilities, caps) {
			t.Fatalf("mask %09b: info %+v", mask, info)
		}
	}
	// A connector that declares nothing, and a remote that declares something unknown.
	if got := has(serve(t, bare{&fullFake{}}, nil)); got != [9]bool{} {
		t.Errorf("bare: %v", got)
	}
	if got := has(serve(t, &fullFake{caps: []connector.Capability{"time_travel", connector.CapRefund}}, nil)); !got[0] {
		t.Errorf("unknown capability hid a known one: %v", got)
	}
}

func TestEveryMethodForwardsItsArguments(t *testing.T) {
	f := &fullFake{caps: allCaps}
	c := serve(t, f, nil)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	co, err := c.CreateCheckout(ctx, connector.CheckoutRequest{PaymentID: "p", ProviderRef: "R", Amount: 49900, Currency: "INR", Customer: connector.Customer{Email: "a@example.com"}})
	must(err)
	if co.ProviderOrderID != "o1" || co.NextAction.URL != "https://x.example.com" {
		t.Errorf("checkout result %+v", co)
	}
	rr, err := c.HandleReturn(ctx, connector.ReturnRequest{ProviderRef: "R", Method: "POST", Form: map[string][]string{"a": {"1"}}})
	must(err)
	if rr.ProviderRef != "R" {
		t.Errorf("return %+v", rr)
	}
	hook := connector.WebhookRequest{Method: "POST", Body: []byte{0, 1, 2, 255}, Headers: map[string][]string{"X-Sig": {"s"}}, ReceivedAt: 7}
	must(c.VerifyWebhook(ctx, hook))
	ns, err := c.ParseWebhook(ctx, hook)
	must(err)
	if len(ns) != 1 || ns[0].Payment.ProviderRef != "r" {
		t.Errorf("notifications %+v", ns)
	}
	po, err := c.(connector.Payouter).CreatePayout(ctx, connector.PayoutRequest{ProviderRef: "PO", Amount: 5000, Currency: "INR",
		Beneficiary: connector.Beneficiary{Name: "B", AccountNumber: "1234567890", IFSC: "HDFC0000001"}})
	must(err)
	pf, err := c.(connector.Payouter).FetchPayout(ctx, connector.PayoutRef{ProviderPayoutID: "po_9"})
	must(err)
	if po.ProviderRef != "PO" || pf.ProviderPayoutID != "po_9" || pf.Status != connector.PayoutSucceeded ||
		f.args["create_payout"].(connector.PayoutRequest).Beneficiary.AccountNumber != "1234567890" {
		t.Errorf("payouts %+v %+v %+v", po, pf, f.args["create_payout"])
	}
	if !reflect.DeepEqual(f.args["verify_webhook"], hook) {
		t.Errorf("binary webhook body altered: %+v", f.args["verify_webhook"])
	}
	ps, err := c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "R"})
	must(err)
	if ps.ProviderRef != "R" || ps.Amount != 5 {
		t.Errorf("payment %+v", ps)
	}

	rf := c.(connector.Refunder)
	r1, err := rf.Refund(ctx, connector.RefundRequest{ProviderRef: "RR", ProviderPaymentID: "pay", Amount: 100, Currency: "INR", Reason: "dup"})
	must(err)
	r2, err := rf.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: "rf9"})
	must(err)
	if r1.ProviderRefundID != "rf1" || r2.ProviderRefundID != "rf9" {
		t.Errorf("refunds %+v %+v", r1, r2)
	}
	dr := c.(connector.DisputeReader)
	dp, err := dr.ListDisputes(ctx, connector.TimeRange{From: 1, To: 2}, "cur")
	must(err)
	if len(dp.Items) != 1 || dp.Next != "n" || !reflect.DeepEqual(f.args["list_disputes"], []any{connector.TimeRange{From: 1, To: 2}, connector.Cursor("cur")}) {
		t.Errorf("disputes %+v / %+v", dp, f.args["list_disputes"])
	}
	d, err := dr.FetchDispute(ctx, connector.DisputeRef{ProviderDisputeID: "d7"})
	must(err)
	if d.ProviderDisputeID != "d7" {
		t.Errorf("dispute %+v", d)
	}
	resp := c.(connector.DisputeResponder)
	must(resp.AcceptDispute(ctx, connector.DisputeRef{ProviderDisputeID: "d1"}))
	must(resp.SubmitEvidence(ctx, connector.DisputeRef{ProviderDisputeID: "d2"}, connector.Evidence{}))
	ls := c.(connector.Lister)
	pp, err := ls.ListPayments(ctx, connector.TimeRange{From: 3, To: 4}, "")
	must(err)
	rp, err := ls.ListRefunds(ctx, connector.TimeRange{From: 5, To: 6}, "c2")
	must(err)
	if len(pp.Items) != 1 || len(rp.Items) != 1 {
		t.Errorf("lists %+v %+v", pp, rp)
	}
	st, err := c.(connector.Settler).ListSettlements(ctx, connector.TimeRange{From: 8, To: 9})
	must(err)
	if len(st) != 1 || st[0].Amount != 9 || f.args["list_settlements"] != (connector.TimeRange{From: 8, To: 9}) {
		t.Errorf("settlements %+v", st)
	}
	must(c.(connector.Canceler).CancelCheckout(ctx, connector.PaymentRef{ProviderRef: "R"}))

	sm := c.(connector.SubscriptionManager)
	sc, err := sm.CreateSubscriptionCheckout(ctx, connector.SubscriptionRequest{SubscriptionID: "sub", ProviderRef: "SR", Amount: 1, Currency: "USD", Interval: "month", IntervalCount: 1})
	must(err)
	ss, err := sm.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: "s1"})
	must(err)
	must(sm.CancelSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: "s1"}, true))
	must(sm.UpdateSubscriptionAmount(ctx, connector.SubscriptionRef{ProviderSubscriptionID: "s1"}, 777))
	if sc.ProviderOrderID != "sub_o" || ss.ProviderSubscriptionID != "s1" ||
		!reflect.DeepEqual(f.args["cancel_subscription"], []any{connector.SubscriptionRef{ProviderSubscriptionID: "s1"}, true}) ||
		!reflect.DeepEqual(f.args["update_subscription_amount"], []any{connector.SubscriptionRef{ProviderSubscriptionID: "s1"}, int64(777)}) {
		t.Errorf("subscriptions %+v %+v %+v", sc, ss, f.args)
	}

	mm := c.(connector.MandateManager)
	mc, err := mm.CreateMandateCheckout(ctx, connector.MandateRequest{ProviderRef: "MR", MaxAmount: 5, Currency: "INR"})
	must(err)
	ms, err := mm.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: "m1"})
	must(err)
	must(mm.NotifyPreDebit(ctx, connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: "m1"}, ProviderRef: "PD", Amount: 5}))
	ch, err := mm.ChargeMandate(ctx, connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: "m1"}, ProviderRef: "CH", Amount: 5, Currency: "INR"})
	must(err)
	must(mm.RevokeMandate(ctx, connector.MandateRef{ProviderMandateID: "m1"}))
	if mc.ProviderOrderID != "m_o" || ms.ProviderMandateID != "m1" || ch.ProviderRef != "CH" {
		t.Errorf("mandates %+v %+v %+v", mc, ms, ch)
	}

	// Every wire method name was exercised exactly once on the server side.
	if len(f.calls) != 26 {
		t.Errorf("server saw %d calls: %v", len(f.calls), f.calls)
	}
}

func TestErrorsKeepTheirTypes(t *testing.T) {
	f := &fullFake{caps: allCaps}
	c := serve(t, f, nil)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"not found":   {connector.ErrNotFound, connector.ErrNotFound},
		"credentials": {connector.ErrInvalidCredentials, connector.ErrInvalidCredentials},
		"temporary":   {connector.ErrTemporary, connector.ErrTemporary},
		"signature":   {connector.ErrInvalidSignature, connector.ErrInvalidSignature},
		"unsigned":    {connector.ErrUnsigned, connector.ErrUnsigned},
		"unsupported": {connector.ErrUnsupported, connector.ErrUnsupported},
		"internal":    {errors.New("boom"), connector.ErrTemporary},
	} {
		f.err = tc.err
		if err := c.VerifyWebhook(ctx, connector.WebhookRequest{}); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f.err = connector.ErrNotRefundable{Reason: "too old"}
	var nr connector.ErrNotRefundable
	if _, err := c.(connector.Refunder).Refund(ctx, connector.RefundRequest{}); !errors.As(err, &nr) || nr.Reason != "too old" {
		t.Errorf("not refundable: %v", err)
	}
	f.err = connector.ErrRateLimited{RetryAfter: 3 * time.Second}
	var rl connector.ErrRateLimited
	if _, err := c.FetchPayment(ctx, connector.PaymentRef{}); !errors.As(err, &rl) || rl.RetryAfter != 3*time.Second {
		t.Errorf("rate limited: %v", err)
	}
	// The remote lacks a capability it was asked for: unsupported.
	c2 := serve(t, bare{&fullFake{}}, nil)
	if _, ok := c2.(connector.Refunder); ok {
		t.Fatal("bare remote claims refunds")
	}
}

func TestSecurity(t *testing.T) {
	f := &fullFake{caps: allCaps}
	h, _ := wire.NewHandler(f, testSecret, func() time.Time { return now0 })
	good := httptest.NewServer(h)
	defer good.Close()

	// A different secret: the remote rejects our requests (401), which discovery reports.
	other := wire.SecretPrefix + base64.StdEncoding.EncodeToString([]byte("a completely different secret!!"))
	cfg, _ := json.Marshal(config{BaseURL: good.URL, Secret: other, AllowInsecure: true, CheckoutHosts: testHosts})
	deps := connectortest.NewDeps(nil, now0)
	deps.HTTP = good.Client()
	if _, err := New(cfg, deps); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("wrong secret: %v", err)
	}

	// A response that was altered on the way back is rejected.
	var tamper bool
	mitm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		b := rec.Body.Bytes()
		if tamper {
			b = append([]byte(nil), b...)
			b[len(b)/2] ^= 1
		}
		_, _ = w.Write(b)
	}))
	defer mitm.Close()
	c := dial(t, mitm, testSecret)
	tamper = true
	if _, err := c.FetchPayment(context.Background(), connector.PaymentRef{}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("tampered response: %v", err)
	}

	// An unsigned 200 (some other service answered) is not trusted either.
	imposter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info") {
			h.ServeHTTP(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"provider_ref":"EVIL","amount":1}`))
	}))
	defer imposter.Close()
	if _, err := dial(t, imposter, testSecret).FetchPayment(context.Background(), connector.PaymentRef{}); !errors.Is(err, connector.ErrInvalidCredentials) {
		t.Errorf("unsigned response: %v", err)
	}

	// A crashed or unreachable service is a retryable failure, not a credentials problem.
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info") {
			h.ServeHTTP(w, r)
			return
		}
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	fc := dial(t, flaky, testSecret)
	if _, err := fc.FetchPayment(context.Background(), connector.PaymentRef{}); !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("502: %v", err)
	}
	flaky.Close()
	if _, err := fc.FetchPayment(context.Background(), connector.PaymentRef{}); !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("connection refused: %v", err)
	}

	// The requests really are signed: a replayed request outside the tolerance is refused.
	sig := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/fetch_payment", nil)
	signer, _ := wire.NewSigner(testSecret)
	signer.SignRequest(req.Header, "POST", "fetch_payment", nil, now0.Add(-time.Hour))
	h.ServeHTTP(sig, req)
	if sig.Code != 401 {
		t.Errorf("stale request: %d", sig.Code)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	deps := connectortest.NewDeps(nil, now0)
	deps.HTTP = http.DefaultClient
	for name, cfg := range map[string]string{
		"not json":   `{`,
		"no url":     fmt.Sprintf(`{"secret":%q,%s}`, testSecret, testHostsJSON),
		"bad scheme": fmt.Sprintf(`{"base_url":"ftp://x.example.com","secret":%q,%s}`, testSecret, testHostsJSON),
		"no secret":  `{"base_url":"https://x.example.com",` + testHostsJSON + `}`,
		"bad secret": `{"base_url":"https://x.example.com","secret":"not base64!",` + testHostsJSON + `}`,
	} {
		if _, err := New(json.RawMessage(cfg), deps); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := New(json.RawMessage(fmt.Sprintf(`{"base_url":"https://x.example.com","secret":%q,%s}`, testSecret, testHostsJSON)), connector.Deps{}); err == nil {
		t.Error("accepted missing HTTP client")
	}
}

func TestRegistered(t *testing.T) {
	_, i, ok := connector.Lookup("remote")
	if !ok || i.Name != "remote" || len(i.ConfigSchema) == 0 {
		t.Fatalf("registration: %+v %v", i, ok)
	}
}

// testHosts is what the test accounts declare: the one place fullFake sends
// the customer.
var testHosts = &connector.CheckoutHosts{Redirect: []string{"https://x.example.com"}}

const testHostsJSON = `"checkout_hosts":{"redirect":["https://x.example.com"]}`

// A remote account must declare where its checkouts may send the customer; the
// hub holds the service to it (audit B-7a).
func TestNewRequiresCheckoutHosts(t *testing.T) {
	deps := connectortest.NewDeps(nil, now0)
	deps.HTTP = http.DefaultClient
	base := fmt.Sprintf(`"base_url":"https://x.example.com","secret":%q`, testSecret)
	for name, hosts := range map[string]string{
		"missing":       ``,
		"null":          `,"checkout_hosts":null`,
		"empty":         `,"checkout_hosts":{}`,
		"empty lists":   `,"checkout_hosts":{"redirect":[]}`,
		"http origin":   `,"checkout_hosts":{"redirect":["http://x.example.com"]}`,
		"path":          `,"checkout_hosts":{"redirect":["https://x.example.com/pay"]}`,
		"keyword":       `,"checkout_hosts":{"script":["'unsafe-inline'"]}`,
		"bare tld wild": `,"checkout_hosts":{"form":["https://*.com"]}`,
		"one bad entry": `,"checkout_hosts":{"redirect":["https://x.example.com","*"]}`,
		"wrong type":    `,"checkout_hosts":["https://x.example.com"]`,
	} {
		_, err := New(json.RawMessage(`{`+base+hosts+`}`), deps)
		if err == nil || !strings.Contains(err.Error(), "checkout_hosts") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRemoteReportsItsCheckoutHosts(t *testing.T) {
	h, err := wire.NewHandler(bare{&fullFake{}}, testSecret, func() time.Time { return now0 })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	deps := connectortest.NewDeps(nil, now0)
	deps.HTTP = srv.Client()
	want := connector.CheckoutHosts{Script: []string{"https://sdk.example.com"}, Form: []string{"https://*.pay.example.com"},
		Redirect: []string{"https://x.example.com"}, Self: "http://evil.example.com"}
	cfg, _ := json.Marshal(map[string]any{"base_url": srv.URL, "secret": testSecret, "allow_insecure": true,
		"checkout_hosts": map[string]any{"script": want.Script, "form": want.Form, "redirect": want.Redirect, "Self": want.Self, "self": want.Self}})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	h := connector.HostsOf(c)
	want.Self = "" // a remote cannot claim the hub's own origin
	if !reflect.DeepEqual(h, want) {
		t.Errorf("hosts = %+v, want %+v", h, want)
	}
}
