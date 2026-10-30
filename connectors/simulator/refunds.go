package simulator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// refRec is one simulated refund.
type refRec struct {
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	PaymentID string `json:"payment_id"`
	PayKey    string `json:"pay_key"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Status    string `json:"status"` // pending | processed
	SettleAt  int64  `json:"settle_at,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func (r refRec) snapshot() connector.RefundSnapshot {
	s := connector.RefundSnapshot{ProviderRef: r.Ref, ProviderRefundID: r.ID, ProviderPaymentID: r.PaymentID, RawStatus: r.Status,
		Amount: r.Amount, Currency: r.Currency, CreatedAt: r.CreatedAt}
	if r.Status == "processed" {
		s.Status = connector.RefundSucceeded
	} else {
		s.Status = connector.RefundPending
	}
	return s
}

func (c *conn) putRefund(ctx context.Context, r *refRec) error {
	if err := c.save(ctx, "ref:"+r.ID, r); err != nil {
		return err
	}
	if r.Ref == "" {
		return nil // a refund the provider made itself has no reference of ours
	}
	return c.saveString(ctx, "ixrefund:"+r.Ref, r.ID)
}

// settleRefund turns a pending refund whose time has come into a processed
// one and adds it to the payment's refunded total. Callers hold mu.
func (c *conn) settleRefund(ctx context.Context, r *refRec) (bool, error) {
	if r.Status != "pending" || c.ms() < r.SettleAt {
		return false, nil
	}
	r.Status, r.SettleAt = "processed", 0
	var p payRec
	if ok, err := c.load(ctx, "pay:"+r.PayKey, &p); err != nil {
		return false, err
	} else if ok {
		p.Refunded += r.Amount
		if err := c.putPay(ctx, &p); err != nil {
			return false, err
		}
	}
	return true, c.putRefund(ctx, r)
}

func (c *conn) findRefund(ctx context.Context, ref connector.RefundRef) (*refRec, error) {
	id := ref.ProviderRefundID
	if id == "" && ref.ProviderRef != "" {
		s, ok, err := c.loadString(ctx, "ixrefund:"+ref.ProviderRef)
		if err != nil {
			return nil, err
		}
		if ok {
			id = s
		}
	}
	if id == "" {
		return nil, connector.ErrNotFound
	}
	var r refRec
	ok, err := c.load(ctx, "ref:"+id, &r)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connector.ErrNotFound
	}
	return &r, nil
}

// Refund is idempotent on ProviderRef. With refund_delay_seconds 0 it succeeds
// at once; otherwise it is pending and succeeds after that many seconds.
func (c *conn) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	mu.Lock()
	if prior, err := c.findRefund(ctx, connector.RefundRef{ProviderRef: req.ProviderRef}); err == nil {
		_, _ = c.settleRefund(ctx, prior)
		mu.Unlock()
		return prior.snapshot(), nil
	}
	p, err := c.findPay(ctx, connector.PaymentRef{ProviderPaymentID: req.ProviderPaymentID})
	if err != nil {
		mu.Unlock()
		return connector.RefundSnapshot{}, err
	}
	switch {
	case p.Status != stCaptured:
		mu.Unlock()
		return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: "the payment is not captured"}
	case req.Amount <= 0 || req.Amount > p.Amount-p.Refunded-c.pendingRefunds(ctx, p.Key):
		mu.Unlock()
		return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: "the amount exceeds what is left to refund"}
	}
	delay := time.Duration(c.cfg.RefundDelaySeconds) * time.Second
	r := &refRec{ID: newID("sim_rfnd_"), Ref: req.ProviderRef, PaymentID: p.PaymentID, PayKey: p.Key, Amount: req.Amount,
		Currency: p.Currency, Status: "pending", SettleAt: c.ms() + delay.Milliseconds(), CreatedAt: c.ms()}
	if err := c.putRefund(ctx, r); err != nil {
		mu.Unlock()
		return connector.RefundSnapshot{}, err
	}
	if err := c.appendList(ctx, "list:refund", r.ID); err != nil {
		mu.Unlock()
		return connector.RefundSnapshot{}, err
	}
	var events []event
	if delay == 0 {
		if _, err := c.settleRefund(ctx, r); err != nil {
			mu.Unlock()
			return connector.RefundSnapshot{}, err
		}
		s := r.snapshot()
		events = append(events, event{Type: "refund.processed", Refund: &s})
	}
	snap := r.snapshot()
	mu.Unlock()
	for _, e := range events {
		c.emit(e)
	}
	if delay > 0 {
		time.AfterFunc(delay+50*time.Millisecond, func() { c.finishRefund(r.ID) })
	}
	return snap, nil
}

// pendingRefunds is what is already promised to be refunded for the payment.
func (c *conn) pendingRefunds(ctx context.Context, payKey string) (total int64) {
	ids, _ := c.list(ctx, "list:refund")
	for _, id := range ids {
		var r refRec
		if ok, _ := c.load(ctx, "ref:"+id, &r); ok && r.PayKey == payKey && r.Status == "pending" {
			total += r.Amount
		}
	}
	return total
}

func (c *conn) finishRefund(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mu.Lock()
	var r refRec
	ok, err := c.load(ctx, "ref:"+id, &r)
	changed := false
	if err == nil && ok {
		changed, err = c.settleRefund(ctx, &r)
	}
	mu.Unlock()
	if err != nil {
		c.log.Warn("simulator: finishing a refund", "err", err)
		return
	}
	if changed {
		s := r.snapshot()
		c.emit(event{Type: "refund.processed", Refund: &s})
	}
}

func (c *conn) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	r, err := c.findRefund(ctx, ref)
	if err != nil {
		return connector.RefundSnapshot{}, err
	}
	if _, err := c.settleRefund(ctx, r); err != nil {
		return connector.RefundSnapshot{}, err
	}
	return r.snapshot(), nil
}

func (c *conn) ListRefunds(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.RefundSnapshot], error) {
	mu.Lock()
	defer mu.Unlock()
	ids, err := c.list(ctx, "list:refund")
	if err != nil {
		return connector.Page[connector.RefundSnapshot]{}, err
	}
	var all []connector.RefundSnapshot
	for _, id := range ids {
		var r refRec
		if ok, err := c.load(ctx, "ref:"+id, &r); err != nil || !ok {
			continue
		}
		_, _ = c.settleRefund(ctx, &r)
		if r.CreatedAt >= tr.From && (tr.To == 0 || r.CreatedAt < tr.To) {
			all = append(all, r.snapshot())
		}
	}
	return page(all, cur), nil
}

// SimulateProviderRefund refunds a captured payment as the provider's own
// dashboard would: at once, with no reference of ours (so Paymux can only learn
// of it from the webhook or from reconciliation). amount 0 refunds the rest.
func (c *conn) SimulateProviderRefund(ctx context.Context, providerPaymentID string, amount int64) (connector.RefundSnapshot, error) {
	mu.Lock()
	p, err := c.findPay(ctx, connector.PaymentRef{ProviderPaymentID: providerPaymentID})
	if err != nil {
		mu.Unlock()
		return connector.RefundSnapshot{}, err
	}
	if p.Status != stCaptured {
		mu.Unlock()
		return connector.RefundSnapshot{}, errors.New("simulator: only a captured payment can be refunded")
	}
	left := p.Amount - p.Refunded - c.pendingRefunds(ctx, p.Key)
	if amount == 0 {
		amount = left
	}
	if amount <= 0 || amount > left {
		mu.Unlock()
		return connector.RefundSnapshot{}, fmt.Errorf("simulator: %d is more than the %d left to refund", amount, left)
	}
	r := &refRec{ID: newID("sim_rfnd_"), PaymentID: p.PaymentID, PayKey: p.Key, Amount: amount, Currency: p.Currency, Status: "pending",
		SettleAt: c.ms(), CreatedAt: c.ms()}
	if err := c.putRefund(ctx, r); err != nil {
		mu.Unlock()
		return connector.RefundSnapshot{}, err
	}
	if err := c.appendList(ctx, "list:refund", r.ID); err != nil {
		mu.Unlock()
		return connector.RefundSnapshot{}, err
	}
	if _, err := c.settleRefund(ctx, r); err != nil {
		mu.Unlock()
		return connector.RefundSnapshot{}, err
	}
	snap := r.snapshot()
	mu.Unlock()
	c.emit(event{Type: "refund.processed", Refund: &snap})
	return snap, nil
}
