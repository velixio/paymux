package simulator

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

var ctx = context.Background()

// harness is a simulator with a fake Paymux behind it: a server that collects
// the signed webhooks the simulator sends.
type harness struct {
	t   *testing.T
	kv  connector.KV
	srv *httptest.Server
	c   *conn
	// verifier is a second connector instance on the same store, standing in for
	// the one Paymux builds when the webhook arrives.
	verifier connector.Connector

	mu    sync.Mutex
	hooks []connector.WebhookRequest
	seen  int
}

func newHarness(t *testing.T, config string) *harness {
	t.Helper()
	h := &harness{t: t, kv: connector.NewMemKV(time.Now)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.hooks = append(h.hooks, connector.WebhookRequest{Method: r.Method, Headers: r.Header.Clone(), Body: b, ReceivedAt: time.Now().UnixMilli()})
		h.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(h.srv.Close)
	deps := connector.Deps{HTTP: http.DefaultClient, KV: h.kv, URLs: connectortest.URLs{Base: h.srv.URL}}
	build := func() connector.Connector {
		c, err := New([]byte(config), deps)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	h.c, h.verifier = build().(*conn), build()
	return h
}

// next waits for the next webhook, checks that it verifies and parses as a
// real inbound webhook would, and returns its notification.
func (h *harness) next(typ string) connector.Notification {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		if h.seen < len(h.hooks) {
			req := h.hooks[h.seen]
			h.seen++
			h.mu.Unlock()
			if err := h.verifier.VerifyWebhook(ctx, req); err != nil {
				h.t.Fatalf("webhook did not verify: %v", err)
			}
			ns, err := h.verifier.ParseWebhook(ctx, req)
			if err != nil || len(ns) != 1 {
				h.t.Fatalf("webhook did not parse: %v %v", ns, err)
			}
			if ns[0].EventType != typ {
				h.t.Fatalf("got webhook %q, want %q", ns[0].EventType, typ)
			}
			return ns[0]
		}
		h.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("no %q webhook within 5s", typ)
	return connector.Notification{}
}

func (h *harness) noMoreHooks(wait time.Duration) {
	h.t.Helper()
	time.Sleep(wait)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen != len(h.hooks) {
		h.t.Errorf("unexpected extra webhook: %d sent, %d expected", len(h.hooks), h.seen)
	}
}

func (h *harness) checkout(ref string, amount int64) connector.CheckoutResult {
	h.t.Helper()
	res, err := h.c.CreateCheckout(ctx, connector.CheckoutRequest{ProviderRef: ref, Amount: amount, Currency: "INR", PaymentID: "pay_x"})
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *harness) fetch(ref string) connector.PaymentSnapshot {
	h.t.Helper()
	s, err := h.c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: ref})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func TestHostedPageFlowSucceed(t *testing.T) {
	h := newHarness(t, `{}`)
	res := h.checkout("ref_a", 49900)
	if res.NextAction.Kind != connector.ActionRedirect || !strings.HasPrefix(res.NextAction.URL, h.srv.URL+"/sim/ref_a?pa=") || res.ProviderOrderID != "sim_ord_ref_a" {
		t.Fatalf("checkout %+v", res)
	}
	// Creating the same checkout again returns the same order.
	if again := h.checkout("ref_a", 49900); again.ProviderOrderID != res.ProviderOrderID {
		t.Error("checkout is not idempotent on the reference")
	}
	st, err := h.c.State(ctx, "ref_a")
	if err != nil || st.Status != stCreated || st.Amount != 49900 || st.Currency != "INR" || st.Kind != kindPayment {
		t.Fatalf("state %+v %v", st, err)
	}
	if got := h.fetch("ref_a"); got.Status != connector.PaymentCreated {
		t.Errorf("before the customer acts: %+v", got)
	}

	if err := h.c.Decide(ctx, "ref_a", ActionSucceed, 0); err != nil {
		t.Fatal(err)
	}
	n := h.next("payment.captured")
	if n.Kind != connector.NotifyPayment || n.Payment.Status != connector.PaymentSucceeded || n.Payment.ProviderRef != "ref_a" || n.Payment.Amount != 49900 {
		t.Errorf("webhook payment %+v", n.Payment)
	}
	got := h.fetch("ref_a")
	if got.Status != connector.PaymentSucceeded || got.ProviderPaymentID == "" || got.Fee != 998 || got.Tax != 180 || got.Method != "simulated" {
		t.Errorf("fetched %+v", got)
	}
	// Found by every id Paymux may hold.
	for _, ref := range []connector.PaymentRef{{ProviderOrderID: "sim_ord_ref_a"}, {ProviderPaymentID: got.ProviderPaymentID}} {
		if s, err := h.c.FetchPayment(ctx, ref); err != nil || s.ProviderPaymentID != got.ProviderPaymentID {
			t.Errorf("fetch by %+v: %+v %v", ref, s, err)
		}
	}
	if _, err := h.c.FetchPayment(ctx, connector.PaymentRef{ProviderRef: "nope"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown payment: %v", err)
	}
	// The return names the payment; an unknown reference is refused.
	rr, err := h.c.HandleReturn(ctx, connector.ReturnRequest{ProviderRef: "ref_a"})
	if err != nil || rr.ProviderRef != "ref_a" || rr.ProviderPaymentID != got.ProviderPaymentID {
		t.Errorf("return %+v %v", rr, err)
	}
	if _, err := h.c.HandleReturn(ctx, connector.ReturnRequest{ProviderRef: "unknown"}); !errors.Is(err, connector.ErrInvalidReturn) {
		t.Errorf("unknown return: %v", err)
	}
	// A decided payment cannot be decided again.
	if err := h.c.Decide(ctx, "ref_a", ActionFail, 0); err == nil {
		t.Error("a captured payment was failed")
	}
	h.noMoreHooks(100 * time.Millisecond)
}

func TestHostedPageOutcomes(t *testing.T) {
	h := newHarness(t, `{}`)

	h.checkout("ref_fail", 1000)
	if err := h.c.Decide(ctx, "ref_fail", ActionFail, 0); err != nil {
		t.Fatal(err)
	}
	n := h.next("payment.failed")
	if n.Payment.Status != connector.PaymentFailed || n.Payment.ErrorCode != "simulated_failure" || n.Payment.ErrorMessage == "" {
		t.Errorf("failure %+v", n.Payment)
	}

	// Authorize only, then capture.
	h.checkout("ref_auth", 2000)
	if err := h.c.Decide(ctx, "ref_auth", ActionAuthorize, 0); err != nil {
		t.Fatal(err)
	}
	h.next("payment.authorized")
	if got := h.fetch("ref_auth"); got.Status != connector.PaymentAuthorized || got.Fee != 0 {
		t.Errorf("authorized: %+v", got)
	}
	if err := h.c.Decide(ctx, "ref_auth", ActionSucceed, 0); err == nil {
		t.Error("an authorised payment was 'succeeded' without a capture")
	}
	if err := h.c.Decide(ctx, "ref_auth", ActionCapture, 0); err != nil {
		t.Fatal(err)
	}
	h.next("payment.captured")
	if got := h.fetch("ref_auth"); got.Status != connector.PaymentSucceeded {
		t.Errorf("captured: %+v", got)
	}

	// Pending, then success after the delay (the timer sends the webhook).
	h.checkout("ref_pend", 3000)
	if err := h.c.Decide(ctx, "ref_pend", ActionPendingSucceed, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := h.fetch("ref_pend"); got.Status != connector.PaymentProcessing {
		t.Errorf("pending: %+v", got)
	}
	start := time.Now()
	h.next("payment.captured")
	if d := time.Since(start); d < 500*time.Millisecond || d > 4*time.Second {
		t.Errorf("success arrived after %v, want about 1s", d)
	}
	if got := h.fetch("ref_pend"); got.Status != connector.PaymentSucceeded {
		t.Errorf("after the delay: %+v", got)
	}

	// A fetch after the delay settles it even if the timer never ran.
	h.checkout("ref_lazy", 4000)
	if err := h.c.Decide(ctx, "ref_lazy", ActionLateSucceed, time.Second); err != nil {
		t.Fatal(err)
	}
	if st, _ := h.c.State(ctx, "ref_lazy"); !st.Late || st.Status != stPending {
		t.Errorf("late state %+v", st)
	}
	h.next("payment.captured")

	// The customer walks away.
	h.checkout("ref_cancel", 5000)
	if err := h.c.Decide(ctx, "ref_cancel", ActionCancel, 0); err != nil {
		t.Fatal(err)
	}
	if got := h.fetch("ref_cancel"); got.Status != connector.PaymentCanceled {
		t.Errorf("canceled: %+v", got)
	}
	h.checkout("ref_cx2", 5000)
	if err := h.c.CancelCheckout(ctx, connector.PaymentRef{ProviderRef: "ref_cx2"}); err != nil || h.fetch("ref_cx2").Status != connector.PaymentCanceled {
		t.Errorf("CancelCheckout: %v", err)
	}
	if err := h.c.Decide(ctx, "ref_cancel", Action("bogus"), 0); err == nil {
		t.Error("unknown action accepted")
	}
	if _, err := h.c.State(ctx, "unknown"); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("state of nothing: %v", err)
	}
}

func TestRefundsInstantAndDelayed(t *testing.T) {
	h := newHarness(t, `{}`)
	h.checkout("ref_r", 10000)
	_ = h.c.Decide(ctx, "ref_r", ActionSucceed, 0)
	h.next("payment.captured")
	pid := h.fetch("ref_r").ProviderPaymentID

	snap, err := h.c.Refund(ctx, connector.RefundRequest{ProviderRef: "rf_1", ProviderPaymentID: pid, Amount: 4000, Currency: "INR"})
	if err != nil || snap.Status != connector.RefundSucceeded || snap.Amount != 4000 || snap.ProviderRefundID == "" {
		t.Fatalf("instant refund %+v %v", snap, err)
	}
	n := h.next("refund.processed")
	if n.Kind != connector.NotifyRefund || n.Refund.ProviderRefundID != snap.ProviderRefundID {
		t.Errorf("refund webhook %+v", n.Refund)
	}
	if again, err := h.c.Refund(ctx, connector.RefundRequest{ProviderRef: "rf_1", ProviderPaymentID: pid, Amount: 4000, Currency: "INR"}); err != nil || again.ProviderRefundID != snap.ProviderRefundID {
		t.Errorf("a repeated refund must return the first: %+v %v", again, err)
	}
	if got := h.fetch("ref_r"); got.AmountRefunded != 4000 {
		t.Errorf("payment after refund: %+v", got)
	}
	if f, err := h.c.FetchRefund(ctx, connector.RefundRef{ProviderRef: "rf_1"}); err != nil || f.Status != connector.RefundSucceeded {
		t.Errorf("FetchRefund: %+v %v", f, err)
	}
	var nr connector.ErrNotRefundable
	if _, err := h.c.Refund(ctx, connector.RefundRequest{ProviderRef: "rf_2", ProviderPaymentID: pid, Amount: 6001, Currency: "INR"}); !errors.As(err, &nr) {
		t.Errorf("refunding more than is left: %v", err)
	}
	h.checkout("ref_unpaid", 1000)
	if _, err := h.c.Refund(ctx, connector.RefundRequest{ProviderRef: "rf_3", ProviderPaymentID: "nope", Amount: 1, Currency: "INR"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("refund of an unknown payment: %v", err)
	}
	page, err := h.c.ListRefunds(ctx, connector.TimeRange{From: 0}, "")
	if err != nil || len(page.Items) != 1 {
		t.Errorf("ListRefunds %+v %v", page, err)
	}

	// A delayed refund is pending first.
	d := newHarness(t, `{"refund_delay_seconds":1}`)
	d.checkout("ref_d", 10000)
	_ = d.c.Decide(ctx, "ref_d", ActionSucceed, 0)
	d.next("payment.captured")
	dpid := d.fetch("ref_d").ProviderPaymentID
	ds, err := d.c.Refund(ctx, connector.RefundRequest{ProviderRef: "rf_d", ProviderPaymentID: dpid, Amount: 1000, Currency: "INR"})
	if err != nil || ds.Status != connector.RefundPending {
		t.Fatalf("delayed refund %+v %v", ds, err)
	}
	if got := d.fetch("ref_d"); got.AmountRefunded != 0 {
		t.Errorf("refunded before it was processed: %+v", got)
	}
	n = d.next("refund.processed")
	if n.Refund.Status != connector.RefundSucceeded {
		t.Errorf("delayed refund webhook %+v", n.Refund)
	}
	if got := d.fetch("ref_d"); got.AmountRefunded != 1000 {
		t.Errorf("after processing: %+v", got)
	}
}

func TestDisputes(t *testing.T) {
	old := evidenceReviewDelay
	evidenceReviewDelay = 200 * time.Millisecond
	t.Cleanup(func() { evidenceReviewDelay = old })
	h := newHarness(t, `{}`)
	h.checkout("ref_x", 20000)
	_ = h.c.Decide(ctx, "ref_x", ActionSucceed, 0)
	h.next("payment.captured")
	pid := h.fetch("ref_x").ProviderPaymentID

	if _, err := h.c.SimulateDispute(ctx, "nope", ""); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("dispute on nothing: %v", err)
	}
	d, err := h.c.SimulateDispute(ctx, pid, "product_not_received")
	if err != nil || d.Status != connector.DisputeNeedsResponse || d.Amount != 20000 || d.Reason != "product_not_received" || d.DueBy <= time.Now().UnixMilli() {
		t.Fatalf("dispute %+v %v", d, err)
	}
	n := h.next("dispute.created")
	if n.Kind != connector.NotifyDispute || n.Dispute.ProviderDisputeID != d.ProviderDisputeID {
		t.Errorf("dispute webhook %+v", n.Dispute)
	}
	if got, err := h.c.FetchDispute(ctx, connector.DisputeRef{ProviderDisputeID: d.ProviderDisputeID}); err != nil || got.Status != connector.DisputeNeedsResponse {
		t.Errorf("FetchDispute %+v %v", got, err)
	}
	if pg, err := h.c.ListDisputes(ctx, connector.TimeRange{From: 0}, ""); err != nil || len(pg.Items) != 1 {
		t.Errorf("ListDisputes %+v %v", pg, err)
	}

	// Evidence: under review, then won.
	if err := h.c.SubmitEvidence(ctx, connector.DisputeRef{ProviderDisputeID: d.ProviderDisputeID}, connector.Evidence{}); err == nil {
		t.Error("empty evidence accepted")
	}
	if err := h.c.SubmitEvidence(ctx, connector.DisputeRef{ProviderDisputeID: d.ProviderDisputeID}, connector.Evidence{Text: "delivered"}); err != nil {
		t.Fatal(err)
	}
	if n := h.next("dispute.under_review"); n.Dispute.Status != connector.DisputeUnderReview {
		t.Errorf("review webhook %+v", n.Dispute)
	}
	if n := h.next("dispute.won"); n.Dispute.Status != connector.DisputeWon {
		t.Errorf("ruling webhook %+v", n.Dispute)
	}
	// A second dispute, conceded.
	d2, err := h.c.SimulateDispute(ctx, pid, "")
	if err != nil {
		t.Fatal(err)
	}
	h.next("dispute.created")
	if err := h.c.AcceptDispute(ctx, connector.DisputeRef{ProviderDisputeID: d2.ProviderDisputeID}); err != nil {
		t.Fatal(err)
	}
	if n := h.next("dispute.lost"); n.Dispute.Status != connector.DisputeLost {
		t.Errorf("accepted dispute %+v", n.Dispute)
	}
	if err := h.c.AcceptDispute(ctx, connector.DisputeRef{ProviderDisputeID: d2.ProviderDisputeID}); err == nil {
		t.Error("a closed dispute was accepted again")
	}
}

func TestMandates(t *testing.T) {
	h := newHarness(t, `{}`)
	res, err := h.c.CreateMandateCheckout(ctx, connector.MandateRequest{MandateID: "mnd_1", ProviderRef: "mref_1", Amount: 19900, MaxAmount: 30000, Currency: "INR"})
	if err != nil || res.ProviderOrderID == "" || !strings.Contains(res.NextAction.URL, "/sim/mref_1") {
		t.Fatalf("mandate checkout %+v %v", res, err)
	}
	connectortest.CheckAllowed(t, h.c, res.NextAction)
	mid := res.ProviderOrderID
	if m, err := h.c.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: mid}); err != nil || m.Status != "pending" || m.SetupPayment != nil {
		t.Fatalf("pending mandate %+v %v", m, err)
	}
	if st, _ := h.c.State(ctx, "mref_1"); st.Kind != kindMandate || st.Amount != 19900 {
		t.Errorf("state %+v", st)
	}
	if err := h.c.Decide(ctx, "mref_1", ActionSucceed, 0); err != nil {
		t.Fatal(err)
	}
	n := h.next("mandate.activated")
	if n.Kind != connector.NotifyMandate || n.Mandate.Status != "active" || n.Mandate.SetupPayment == nil || n.Mandate.SetupPayment.ProviderRef != "mref_1" ||
		n.Mandate.SetupPayment.Status != connector.PaymentSucceeded || n.Mandate.SetupPayment.Amount != 19900 {
		t.Errorf("mandate webhook %+v", n.Mandate)
	}
	m, err := h.c.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: mid})
	if err != nil || m.Status != "active" || m.MaxAmount != 30000 || m.SetupPayment == nil {
		t.Fatalf("active mandate %+v %v", m, err)
	}
	if err := h.c.NotifyPreDebit(ctx, connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: mid}, ProviderRef: "c1", Amount: 19900, Currency: "INR"}); err != nil {
		t.Errorf("pre-debit notice: %v", err)
	}
	cs, err := h.c.ChargeMandate(ctx, connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: mid}, ProviderRef: "charge_1", Amount: 19900, Currency: "INR"})
	if err != nil || cs.Status != connector.PaymentSucceeded || cs.ProviderRef != "charge_1" || cs.ProviderPaymentID == "" {
		t.Fatalf("charge %+v %v", cs, err)
	}
	if again, _ := h.c.ChargeMandate(ctx, connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: mid}, ProviderRef: "charge_1", Amount: 19900, Currency: "INR"}); again.ProviderPaymentID != cs.ProviderPaymentID {
		t.Error("a repeated charge with the same reference made a second payment")
	}
	if got := h.fetch("charge_1"); got.Status != connector.PaymentSucceeded {
		t.Errorf("charge fetched by reference: %+v", got)
	}
	if _, err := h.c.ChargeMandate(ctx, connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: mid}, ProviderRef: "charge_big", Amount: 30001, Currency: "INR"}); !errors.Is(err, connector.ErrMandateLimit) {
		t.Errorf("over the maximum: %v", err)
	}
	if err := h.c.RevokeMandate(ctx, connector.MandateRef{ProviderMandateID: mid}); err != nil {
		t.Fatal(err)
	}
	cs, err = h.c.ChargeMandate(ctx, connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: mid}, ProviderRef: "charge_2", Amount: 100, Currency: "INR"})
	if err != nil || cs.Status != connector.PaymentFailed || cs.ErrorCode != "mandate_rejected" {
		t.Errorf("charge on a revoked mandate %+v %v", cs, err)
	}
	if m, _ := h.c.FetchMandate(ctx, connector.MandateRef{ProviderMandateID: mid}); m.Status != "revoked" {
		t.Errorf("revoked mandate %+v", m)
	}

	// A failed registration.
	res, _ = h.c.CreateMandateCheckout(ctx, connector.MandateRequest{ProviderRef: "mref_2", MaxAmount: 1000, Currency: "INR"})
	if err := h.c.Decide(ctx, "mref_2", ActionFail, 0); err != nil {
		t.Fatal(err)
	}
	if n := h.next("mandate.failed"); n.Mandate.Status != "failed" {
		t.Errorf("failed mandate %+v", n.Mandate)
	}
	if st, _ := h.c.State(ctx, "mref_2"); st.Amount != info.MandateSetupAmount {
		t.Errorf("a registration with no amount debits the setup amount: %+v", st)
	}
}

func TestProviderManagedSubscriptions(t *testing.T) {
	h := newHarness(t, `{}`)
	res, err := h.c.CreateSubscriptionCheckout(ctx, connector.SubscriptionRequest{SubscriptionID: "sub_1", ProviderRef: "sref_1", Amount: 49900, Currency: "INR", Interval: "month", IntervalCount: 1})
	if err != nil || res.ProviderOrderID == "" || !strings.Contains(res.NextAction.URL, "/sim/sref_1") {
		t.Fatalf("subscription checkout %+v %v", res, err)
	}
	connectortest.CheckAllowed(t, h.c, res.NextAction)
	sid := res.ProviderOrderID
	if s, err := h.c.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sid}); err != nil || s.Status != "incomplete" || s.ProviderRef != "sref_1" {
		t.Fatalf("before authorisation %+v %v", s, err)
	}
	if err := h.c.Decide(ctx, "sref_1", ActionSucceed, 0); err != nil {
		t.Fatal(err)
	}
	n := h.next("subscription.charged")
	if n.Kind != connector.NotifySubscription || n.Subscription.Status != "active" || n.Payment == nil || n.Payment.SubscriptionID != sid ||
		n.Payment.Status != connector.PaymentSucceeded || n.Payment.Amount != 49900 || n.Payment.ProviderPaymentID == "" || n.Payment.ProviderRef != "" {
		t.Errorf("subscription webhook %+v / %+v", n.Subscription, n.Payment)
	}
	first := n.Payment.ProviderPaymentID
	s, err := h.c.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sid})
	if err != nil || s.Status != "active" || s.CurrentPeriodStart == 0 || s.CurrentPeriodEnd <= s.CurrentPeriodStart {
		t.Fatalf("active %+v %v", s, err)
	}
	if got, err := h.c.FetchPayment(ctx, connector.PaymentRef{ProviderPaymentID: first}); err != nil || got.SubscriptionID != sid {
		t.Errorf("first charge %+v %v", got, err)
	}

	if err := h.c.SimulateRenewal(ctx, sid, false); err != nil {
		t.Fatal(err)
	}
	n = h.next("subscription.charged")
	if n.Payment.ProviderPaymentID == first || n.Payment.SubscriptionID != sid {
		t.Errorf("renewal %+v", n.Payment)
	}
	if err := h.c.SimulateRenewal(ctx, sid, true); err != nil {
		t.Fatal(err)
	}
	if n := h.next("subscription.halted"); n.Subscription.Status != "past_due" || n.Payment != nil {
		t.Errorf("failed renewal %+v", n.Subscription)
	}
	// A successful renewal brings it back.
	if err := h.c.SimulateRenewal(ctx, sid, false); err != nil {
		t.Fatal(err)
	}
	if n := h.next("subscription.charged"); n.Subscription.Status != "active" {
		t.Errorf("recovered %+v", n.Subscription)
	}
	if err := h.c.UpdateSubscriptionAmount(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sid}, 59900); err != nil {
		t.Fatal(err)
	}
	_ = h.c.SimulateRenewal(ctx, sid, false)
	if n := h.next("subscription.charged"); n.Payment.Amount != 59900 {
		t.Errorf("new amount not used: %+v", n.Payment)
	}
	// Cancel at period end: the next renewal ends it.
	if err := h.c.CancelSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sid}, true); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.c.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: sid}); s.Status != "active" {
		t.Errorf("cancel at period end ended it at once: %+v", s)
	}
	_ = h.c.SimulateRenewal(ctx, sid, false)
	if n := h.next("subscription.cancelled"); n.Subscription.Status != "canceled" {
		t.Errorf("ended %+v", n.Subscription)
	}
	if err := h.c.SimulateRenewal(ctx, sid, false); err == nil {
		t.Error("a canceled subscription renewed")
	}

	// Immediate cancel, and a declined authorisation.
	res, _ = h.c.CreateSubscriptionCheckout(ctx, connector.SubscriptionRequest{ProviderRef: "sref_2", Amount: 100, Currency: "USD", Interval: "week", IntervalCount: 2})
	_ = h.c.Decide(ctx, "sref_2", ActionSucceed, 0)
	h.next("subscription.charged")
	if err := h.c.CancelSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: res.ProviderOrderID}, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.c.FetchSubscription(ctx, connector.SubscriptionRef{ProviderSubscriptionID: res.ProviderOrderID}); s.Status != "canceled" {
		t.Errorf("immediate cancel %+v", s)
	}
	_, _ = h.c.CreateSubscriptionCheckout(ctx, connector.SubscriptionRequest{ProviderRef: "sref_3", Amount: 100, Currency: "INR", Interval: "day", IntervalCount: 1})
	_ = h.c.Decide(ctx, "sref_3", ActionFail, 0)
	if n := h.next("subscription.cancelled"); n.Subscription.RawStatus != "failed" {
		t.Errorf("declined authorisation %+v", n.Subscription)
	}
}

func TestListsAndSettlements(t *testing.T) {
	h := newHarness(t, `{}`)
	for _, ref := range []string{"l1", "l2", "l3"} {
		h.checkout(ref, 10000)
	}
	_ = h.c.Decide(ctx, "l1", ActionSucceed, 0)
	h.next("payment.captured")
	_ = h.c.Decide(ctx, "l2", ActionFail, 0)
	h.next("payment.failed")
	pg, err := h.c.ListPayments(ctx, connector.TimeRange{From: 0}, "")
	if err != nil || len(pg.Items) != 2 { // l3 was never paid, so it is not in the provider's list
		t.Fatalf("ListPayments %d %v", len(pg.Items), err)
	}
	if got, _ := h.c.ListPayments(ctx, connector.TimeRange{From: time.Now().Add(time.Hour).UnixMilli()}, ""); len(got.Items) != 0 {
		t.Error("range not applied")
	}
	pid := h.fetch("l1").ProviderPaymentID
	if _, err := h.c.Refund(ctx, connector.RefundRequest{ProviderRef: "rfl", ProviderPaymentID: pid, Amount: 2500, Currency: "INR"}); err != nil {
		t.Fatal(err)
	}
	h.next("refund.processed")
	sets, err := h.c.ListSettlements(ctx, connector.TimeRange{From: 0})
	if err != nil || len(sets) != 1 {
		t.Fatalf("settlements %+v %v", sets, err)
	}
	s := sets[0]
	// 10000 gross: fee 200 + tax 36 -> net 9764, less the 2500 refund.
	if s.Fees != 200 || s.Tax != 36 || s.Amount != 9764-2500 || s.Currency != "INR" || s.Status != "processed" || s.UTR == "" || len(s.Items) != 2 {
		t.Errorf("settlement %+v", s)
	}
	if s.Items[0].Kind != "payment" || s.Items[0].Fee != 200 || s.Items[0].Tax != 36 || s.Items[1].Kind != "refund" || s.Items[1].Amount != -2500 {
		t.Errorf("items %+v", s.Items)
	}

	// Paging: 101 payments need two pages.
	for i := 0; i < 101; i++ {
		ref := "bulk" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+i/26))
		h.checkout(ref, 100)
		_ = h.c.Decide(ctx, ref, ActionSucceed, 0)
	}
	first, _ := h.c.ListPayments(ctx, connector.TimeRange{From: 0}, "")
	if len(first.Items) != 100 || first.Next == "" {
		t.Fatalf("first page %d next %q", len(first.Items), first.Next)
	}
	second, _ := h.c.ListPayments(ctx, connector.TimeRange{From: 0}, first.Next)
	if len(second.Items) != 3 || second.Next != "" {
		t.Errorf("second page %d next %q", len(second.Items), second.Next)
	}
}

func TestWebhookSecretIsPerAccountAndPersistent(t *testing.T) {
	a, b := newHarness(t, `{}`), newHarness(t, `{}`)
	body := []byte(`{"id":"evt_sim_9","type":"payment.captured","payment":{"provider_ref":"r","status":"succeeded","raw_status":"captured","amount":1,"currency":"INR"}}`)
	sec, err := a.c.secret(ctx)
	if err != nil || !strings.HasPrefix(sec, "simsec_") {
		t.Fatalf("secret %q %v", sec, err)
	}
	if again, _ := a.verifier.(*conn).secret(ctx); again != sec {
		t.Error("another instance on the same store sees a different secret")
	}
	ts := time.Now().Unix()
	req := connector.WebhookRequest{Body: body, Headers: map[string][]string{sigHeader: {sign(sec, ts, body)}, timestampHeader: {itoa(ts)}}}
	if err := a.verifier.VerifyWebhook(ctx, req); err != nil {
		t.Errorf("own secret: %v", err)
	}
	if err := b.verifier.VerifyWebhook(ctx, req); !errors.Is(err, connector.ErrInvalidSignature) {
		t.Errorf("another account's webhook verified: %v", err)
	}
	for _, mut := range []func(*connector.WebhookRequest){
		func(r *connector.WebhookRequest) { r.Body = append([]byte(nil), r.Body...); r.Body[3] = 'X' },
		func(r *connector.WebhookRequest) {
			r.Headers = map[string][]string{timestampHeader: r.Headers[timestampHeader]}
		},
		func(r *connector.WebhookRequest) { r.Headers = map[string][]string{sigHeader: r.Headers[sigHeader]} },
		func(r *connector.WebhookRequest) {
			r.Headers = map[string][]string{sigHeader: {"zz"}, timestampHeader: {"notanumber"}}
		},
	} {
		bad := req
		mut(&bad)
		if err := a.verifier.VerifyWebhook(ctx, bad); !errors.Is(err, connector.ErrInvalidSignature) || errors.Is(err, connector.ErrUnsigned) {
			t.Errorf("tampered webhook: %v", err)
		}
	}
	if _, err := a.verifier.ParseWebhook(ctx, connector.WebhookRequest{Body: []byte(`{"id":"e","type":"x"}`)}); err == nil {
		t.Error("a webhook with no object parsed")
	}
}

func TestConfigAndInfo(t *testing.T) {
	if _, err := New([]byte(`{"refund_delay_seconds":-1}`), connector.Deps{KV: connector.NewMemKV(time.Now)}); err == nil {
		t.Error("negative refund delay accepted")
	}
	if _, err := New([]byte(`{}`), connector.Deps{}); err == nil {
		t.Error("a connector without a key/value store was built")
	}
	i := (&conn{}).Info()
	if !i.TestOnly || i.MerchantOfRecord || i.Name != "simulator" {
		t.Errorf("info %+v", i)
	}
	for _, cur := range []string{"INR", "USD", "BDT"} {
		found := false
		for _, c := range i.Currencies {
			found = found || c == cur
		}
		if !found {
			t.Errorf("currency %s missing", cur)
		}
	}
	for _, want := range []connector.Capability{connector.CapRefund, connector.CapDisputeRead, connector.CapDisputeRespond, connector.CapList, connector.CapSettle,
		connector.CapCancel, connector.CapSubscriptionManager, connector.CapMandateManager} {
		if !i.Has(want) {
			t.Errorf("capability %s not declared", want)
		}
	}
	h := newHarness(t, `{}`)
	if _, err := h.c.CreateCheckout(ctx, connector.CheckoutRequest{ProviderRef: "x", Amount: 100, Currency: "JPY"}); err == nil {
		t.Error("an unsupported currency was accepted")
	}
	if _, err := h.c.CreateCheckout(ctx, connector.CheckoutRequest{Amount: 100, Currency: "INR"}); err == nil {
		t.Error("a checkout without a reference was accepted")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestProviderRefundsAreMadeBehindPaymuxsBack(t *testing.T) {
	h := newHarness(t, `{}`)
	h.checkout("ref_pr", 10000)
	_ = h.c.Decide(ctx, "ref_pr", ActionSucceed, 0)
	h.next("payment.captured")
	pid := h.fetch("ref_pr").ProviderPaymentID

	snap, err := h.c.SimulateProviderRefund(ctx, pid, 2500)
	if err != nil || snap.Status != connector.RefundSucceeded || snap.Amount != 2500 || snap.ProviderPaymentID != pid || snap.ProviderRef != "" {
		t.Fatalf("refund %+v %v", snap, err)
	}
	n := h.next("refund.processed")
	if n.Refund.ProviderRefundID != snap.ProviderRefundID || n.Refund.ProviderRef != "" {
		t.Errorf("webhook %+v", n.Refund)
	}
	// It is the provider's truth now: fetchable by its id, counted on the payment, listed.
	got, err := h.c.FetchRefund(ctx, connector.RefundRef{ProviderRefundID: snap.ProviderRefundID})
	if err != nil || got.Status != connector.RefundSucceeded || got.Amount != 2500 {
		t.Errorf("FetchRefund %+v %v", got, err)
	}
	if p := h.fetch("ref_pr"); p.AmountRefunded != 2500 {
		t.Errorf("payment %+v", p)
	}
	// Amount 0 refunds what is left; more than that is refused; so is an uncaptured payment.
	if _, err := h.c.SimulateProviderRefund(ctx, pid, 7501); err == nil {
		t.Error("more than is left was refunded")
	}
	rest, err := h.c.SimulateProviderRefund(ctx, pid, 0)
	if err != nil || rest.Amount != 7500 {
		t.Errorf("the rest: %+v %v", rest, err)
	}
	h.next("refund.processed")
	h.checkout("ref_unpaid2", 100)
	if _, err := h.c.SimulateProviderRefund(ctx, "nope", 1); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown payment: %v", err)
	}
	// Our own refund of what is left is now impossible.
	var nr connector.ErrNotRefundable
	if _, err := h.c.Refund(ctx, connector.RefundRequest{ProviderRef: "late", ProviderPaymentID: pid, Amount: 1, Currency: "INR"}); !errors.As(err, &nr) {
		t.Errorf("refund after the provider refunded everything: %v", err)
	}
}
