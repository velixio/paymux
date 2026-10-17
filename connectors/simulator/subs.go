package simulator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// ---- records ----

type mandateRec struct {
	ID         string `json:"id"`
	Ref        string `json:"ref"`
	PayKey     string `json:"pay_key"` // the registration debit
	Status     string `json:"status"`  // pending | active | revoked | expired | failed
	MaxAmount  int64  `json:"max_amount"`
	Currency   string `json:"currency"`
	Method     string `json:"method"`
	ValidUntil int64  `json:"valid_until,omitempty"`
}

func (c *conn) mandateSnapshot(ctx context.Context, m mandateRec) connector.MandateSnapshot {
	s := connector.MandateSnapshot{ProviderMandateID: m.ID, Status: m.Status, RawStatus: m.Status, MaxAmount: m.MaxAmount,
		Currency: m.Currency, Method: m.Method, ValidUntil: m.ValidUntil}
	var p payRec
	if ok, _ := c.load(ctx, "pay:"+m.PayKey, &p); ok && (p.Status == stCaptured || p.Status == stFailed) {
		ps := p.snapshot()
		s.SetupPayment = &ps
	}
	return s
}

type subRec struct {
	ID            string `json:"id"`
	Ref           string `json:"ref"`
	PayKey        string `json:"pay_key"` // the authorisation checkout
	Status        string `json:"status"`  // created | active | past_due | canceled
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Interval      string `json:"interval"`
	IntervalCount int    `json:"interval_count"`
	Renewals      int    `json:"renewals"` // charges so far, the first included
	PeriodStart   int64  `json:"period_start,omitempty"`
	CancelAtEnd   bool   `json:"cancel_at_end,omitempty"`
}

func addInterval(t time.Time, interval string, count int) time.Time {
	if count < 1 {
		count = 1
	}
	switch interval {
	case "day":
		return t.AddDate(0, 0, count)
	case "week":
		return t.AddDate(0, 0, 7*count)
	case "year":
		return t.AddDate(count, 0, 0)
	}
	return t.AddDate(0, count, 0)
}

func (s subRec) snapshot() connector.SubscriptionSnapshot {
	snap := connector.SubscriptionSnapshot{ProviderSubscriptionID: s.ID, ProviderRef: s.Ref, RawStatus: s.Status, Amount: s.Amount, Currency: s.Currency}
	switch s.Status {
	case "active":
		snap.Status = "active"
	case "past_due":
		snap.Status = "past_due"
	case "canceled":
		snap.Status = "canceled"
	default:
		snap.Status, snap.RawStatus = "incomplete", "created"
	}
	if s.PeriodStart > 0 {
		start := time.UnixMilli(s.PeriodStart)
		snap.CurrentPeriodStart = s.PeriodStart
		snap.CurrentPeriodEnd = addInterval(start, s.Interval, s.IntervalCount).UnixMilli()
	}
	return snap
}

func (c *conn) putMandate(ctx context.Context, m *mandateRec) error {
	if err := c.save(ctx, "mnd:"+m.ID, m); err != nil {
		return err
	}
	return c.saveString(ctx, "ixmnd:"+m.Ref, m.ID)
}

func (c *conn) putSub(ctx context.Context, s *subRec) error {
	if err := c.save(ctx, "sub:"+s.ID, s); err != nil {
		return err
	}
	return c.saveString(ctx, "ixsub:"+s.Ref, s.ID)
}

func (c *conn) getMandate(ctx context.Context, id string) (*mandateRec, error) {
	var m mandateRec
	ok, err := c.load(ctx, "mnd:"+id, &m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connector.ErrNotFound
	}
	return &m, nil
}

func (c *conn) getSub(ctx context.Context, id string) (*subRec, error) {
	var s subRec
	ok, err := c.load(ctx, "sub:"+id, &s)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connector.ErrNotFound
	}
	return &s, nil
}

// ---- what happens when a checkout payment reaches a final state ----

// afterCapture runs the consequences of p being captured and returns the
// webhooks to send when wantEvents is set. Callers hold mu and save p.
func (c *conn) afterCapture(ctx context.Context, p *payRec, wantEvents bool) ([]event, error) {
	var events []event
	switch p.Kind {
	case kindPayment:
		events = append(events, c.payEvent("payment.captured", *p))
	case kindMandate:
		m, err := c.getMandate(ctx, p.MandateID)
		if err != nil {
			return nil, err
		}
		m.Status = "active"
		if err := c.putMandate(ctx, m); err != nil {
			return nil, err
		}
		s := c.mandateSnapshotWith(*m, *p)
		events = append(events, event{Type: "mandate.activated", Mandate: &s})
	case kindSubFirst:
		sub, err := c.getSub(ctx, p.SubID)
		if err != nil {
			return nil, err
		}
		ev, err := c.chargeSub(ctx, sub, "subscription.charged")
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	if !wantEvents {
		return nil, nil
	}
	return events, nil
}

func (c *conn) mandateSnapshotWith(m mandateRec, p payRec) connector.MandateSnapshot {
	ps := p.snapshot()
	return connector.MandateSnapshot{ProviderMandateID: m.ID, Status: m.Status, RawStatus: m.Status, MaxAmount: m.MaxAmount,
		Currency: m.Currency, Method: m.Method, ValidUntil: m.ValidUntil, SetupPayment: &ps}
}

// afterFailure runs the consequences of p failing.
func (c *conn) afterFailure(ctx context.Context, p *payRec) ([]event, error) {
	switch p.Kind {
	case kindMandate:
		m, err := c.getMandate(ctx, p.MandateID)
		if err != nil {
			return nil, err
		}
		m.Status = "failed"
		if err := c.putMandate(ctx, m); err != nil {
			return nil, err
		}
		s := c.mandateSnapshotWith(*m, *p)
		return []event{{Type: "mandate.failed", Mandate: &s}}, nil
	case kindSubFirst:
		sub, err := c.getSub(ctx, p.SubID)
		if err != nil {
			return nil, err
		}
		sub.Status = "canceled"
		if err := c.putSub(ctx, sub); err != nil {
			return nil, err
		}
		s := sub.snapshot()
		s.RawStatus = "failed"
		return []event{{Type: "subscription.cancelled", Subscription: &s}}, nil
	}
	return []event{c.payEvent("payment.failed", *p)}, nil
}

// chargeSub records one successful charge of a managed subscription: a new
// captured payment (which carries the subscription id), a new period, and the
// webhook with both.
func (c *conn) chargeSub(ctx context.Context, sub *subRec, typ string) (event, error) {
	pay, err := c.newPay(ctx, payRec{Kind: kindCharge, PaymentID: newID("sim_pay_"), Amount: sub.Amount, Currency: sub.Currency,
		Status: stCaptured, Method: "simulated", SubID: sub.ID})
	if err != nil {
		return event{}, err
	}
	sub.Status, sub.PeriodStart = "active", c.ms()
	sub.Renewals++
	if err := c.putSub(ctx, sub); err != nil {
		return event{}, err
	}
	ss, ps := sub.snapshot(), pay.snapshot()
	return event{Type: typ, Subscription: &ss, Payment: &ps}, nil
}

// ---- MandateManager ----

func (c *conn) CreateMandateCheckout(ctx context.Context, req connector.MandateRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || req.MaxAmount <= 0 {
		return connector.CheckoutResult{}, errors.New("simulator: a mandate needs a provider_ref and a positive max amount")
	}
	if !supports(req.Currency) {
		return connector.CheckoutResult{}, fmt.Errorf("simulator: currency %q is not supported", req.Currency)
	}
	page, err := c.pageURL(req.ProviderRef)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	amount := req.Amount
	if amount <= 0 {
		amount = info.MandateSetupAmount
	}
	method := req.Method
	if method == "" {
		method = "upi"
	}
	mu.Lock()
	defer mu.Unlock()
	if id, ok, err := c.loadString(ctx, "ixmnd:"+req.ProviderRef); err != nil {
		return connector.CheckoutResult{}, err
	} else if ok {
		return connector.CheckoutResult{ProviderOrderID: id, NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: page}}, nil
	}
	m := &mandateRec{ID: newID("sim_mnd_"), Ref: req.ProviderRef, Status: "pending", MaxAmount: req.MaxAmount, Currency: req.Currency, Method: method, ValidUntil: req.ValidUntil}
	p, err := c.newPay(ctx, payRec{Kind: kindMandate, Ref: req.ProviderRef, OrderID: m.ID, Amount: amount, Currency: req.Currency, MandateID: m.ID})
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	m.PayKey = p.Key
	if err := c.putMandate(ctx, m); err != nil {
		return connector.CheckoutResult{}, err
	}
	return connector.CheckoutResult{ProviderOrderID: m.ID, NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: page}}, nil
}

func (c *conn) FetchMandate(ctx context.Context, ref connector.MandateRef) (connector.MandateSnapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	m, err := c.getMandate(ctx, ref.ProviderMandateID)
	if err != nil {
		return connector.MandateSnapshot{}, err
	}
	// A registration debit that was pending has to be settled first.
	var p payRec
	if ok, _ := c.load(ctx, "pay:"+m.PayKey, &p); ok && p.due(c.ms()) {
		if _, err := c.afterCapture(ctx, &p, false); err != nil {
			return connector.MandateSnapshot{}, err
		}
		if err := c.putPay(ctx, &p); err != nil {
			return connector.MandateSnapshot{}, err
		}
		if m, err = c.getMandate(ctx, ref.ProviderMandateID); err != nil {
			return connector.MandateSnapshot{}, err
		}
	}
	return c.mandateSnapshot(ctx, *m), nil
}

// NotifyPreDebit accepts the notice: the simulated customer is always told.
func (c *conn) NotifyPreDebit(ctx context.Context, req connector.PreDebitRequest) error {
	mu.Lock()
	defer mu.Unlock()
	if _, err := c.getMandate(ctx, req.Mandate.ProviderMandateID); err != nil {
		return err
	}
	return nil
}

// ChargeMandate debits an active mandate at once. ProviderRef makes it
// idempotent. A debit above the mandate's maximum is ErrMandateLimit; one on
// a mandate that is not active is a failed payment.
func (c *conn) ChargeMandate(ctx context.Context, req connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	if req.ProviderRef == "" || req.Amount <= 0 {
		return connector.PaymentSnapshot{}, errors.New("simulator: a charge needs a provider_ref and a positive amount")
	}
	mu.Lock()
	defer mu.Unlock()
	if p, err := c.findPay(ctx, connector.PaymentRef{ProviderRef: req.ProviderRef}); err == nil {
		return p.snapshot(), nil // the same charge again
	}
	m, err := c.getMandate(ctx, req.Mandate.ProviderMandateID)
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	if req.Amount > m.MaxAmount {
		return connector.PaymentSnapshot{}, connector.ErrMandateLimit
	}
	rec := payRec{Kind: kindCharge, Ref: req.ProviderRef, PaymentID: newID("sim_pay_"), Amount: req.Amount, Currency: req.Currency,
		Status: stCaptured, Method: "upi", MandateID: m.ID}
	if m.Status != "active" {
		rec.Status, rec.ErrCode, rec.ErrMsg = stFailed, "mandate_rejected", "the mandate is "+m.Status
	}
	p, err := c.newPay(ctx, rec)
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	return p.snapshot(), nil
}

func (c *conn) RevokeMandate(ctx context.Context, ref connector.MandateRef) error {
	mu.Lock()
	defer mu.Unlock()
	m, err := c.getMandate(ctx, ref.ProviderMandateID)
	if err != nil {
		return err
	}
	if m.Status == "revoked" {
		return nil
	}
	m.Status = "revoked"
	return c.putMandate(ctx, m)
}

// ---- SubscriptionManager (provider-managed billing) ----

func (c *conn) CreateSubscriptionCheckout(ctx context.Context, req connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || req.Amount <= 0 {
		return connector.CheckoutResult{}, errors.New("simulator: a subscription needs a provider_ref and a positive amount")
	}
	if !supports(req.Currency) {
		return connector.CheckoutResult{}, fmt.Errorf("simulator: currency %q is not supported", req.Currency)
	}
	page, err := c.pageURL(req.ProviderRef)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	mu.Lock()
	defer mu.Unlock()
	if id, ok, err := c.loadString(ctx, "ixsub:"+req.ProviderRef); err != nil {
		return connector.CheckoutResult{}, err
	} else if ok {
		return connector.CheckoutResult{ProviderOrderID: id, NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: page}}, nil
	}
	sub := &subRec{ID: newID("sim_sub_"), Ref: req.ProviderRef, Status: "created", Amount: req.Amount, Currency: req.Currency,
		Interval: req.Interval, IntervalCount: req.IntervalCount}
	p, err := c.newPay(ctx, payRec{Kind: kindSubFirst, Ref: req.ProviderRef, OrderID: sub.ID, Amount: req.Amount, Currency: req.Currency, SubID: sub.ID})
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	sub.PayKey = p.Key
	if err := c.putSub(ctx, sub); err != nil {
		return connector.CheckoutResult{}, err
	}
	return connector.CheckoutResult{ProviderOrderID: sub.ID, NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: page}}, nil
}

func (c *conn) FetchSubscription(ctx context.Context, ref connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	s, err := c.getSub(ctx, ref.ProviderSubscriptionID)
	if err != nil {
		return connector.SubscriptionSnapshot{}, err
	}
	var p payRec
	if ok, _ := c.load(ctx, "pay:"+s.PayKey, &p); ok && p.due(c.ms()) {
		if _, err := c.afterCapture(ctx, &p, false); err != nil {
			return connector.SubscriptionSnapshot{}, err
		}
		if err := c.putPay(ctx, &p); err != nil {
			return connector.SubscriptionSnapshot{}, err
		}
		if s, err = c.getSub(ctx, ref.ProviderSubscriptionID); err != nil {
			return connector.SubscriptionSnapshot{}, err
		}
	}
	return s.snapshot(), nil
}

func (c *conn) CancelSubscription(ctx context.Context, ref connector.SubscriptionRef, atPeriodEnd bool) error {
	mu.Lock()
	defer mu.Unlock()
	s, err := c.getSub(ctx, ref.ProviderSubscriptionID)
	if err != nil {
		return err
	}
	if atPeriodEnd {
		s.CancelAtEnd = true
	} else {
		s.Status = "canceled"
	}
	return c.putSub(ctx, s)
}

func (c *conn) UpdateSubscriptionAmount(ctx context.Context, ref connector.SubscriptionRef, amount int64) error {
	if amount <= 0 {
		return errors.New("simulator: the amount must be positive")
	}
	mu.Lock()
	defer mu.Unlock()
	s, err := c.getSub(ctx, ref.ProviderSubscriptionID)
	if err != nil {
		return err
	}
	s.Amount = amount
	return c.putSub(ctx, s)
}

// SimulateRenewal makes an active (or past due) subscription charge its next
// period, or fail to. A subscription set to cancel at period end ends instead.
func (c *conn) SimulateRenewal(ctx context.Context, providerSubscriptionID string, fail bool) error {
	mu.Lock()
	s, err := c.getSub(ctx, providerSubscriptionID)
	if err != nil {
		mu.Unlock()
		return err
	}
	if s.Status != "active" && s.Status != "past_due" {
		mu.Unlock()
		return fmt.Errorf("simulator: the subscription is %s, not active", s.Status)
	}
	var ev event
	switch {
	case s.CancelAtEnd:
		s.Status = "canceled"
		err = c.putSub(ctx, s)
		snap := s.snapshot()
		ev = event{Type: "subscription.cancelled", Subscription: &snap}
	case fail:
		s.Status = "past_due"
		err = c.putSub(ctx, s)
		snap := s.snapshot()
		ev = event{Type: "subscription.halted", Subscription: &snap}
	default:
		ev, err = c.chargeSub(ctx, s, "subscription.charged")
	}
	mu.Unlock()
	if err != nil {
		return err
	}
	c.emit(ev)
	return nil
}
