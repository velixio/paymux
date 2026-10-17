package simulator

import (
	"context"
	"errors"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// evidenceReviewDelay is how long a dispute stays under review after evidence
// is submitted before the bank rules in the merchant's favour.
var evidenceReviewDelay = 10 * time.Second

// disputeRec is one simulated dispute.
type disputeRec struct {
	ID        string `json:"id"`
	PaymentID string `json:"payment_id"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Status    string `json:"status"` // a connector.DisputeStatus
	Reason    string `json:"reason"`
	DueBy     int64  `json:"due_by"`
	CreatedAt int64  `json:"created_at"`
	// WinAt: an under-review dispute is won at this time.
	WinAt int64 `json:"win_at,omitempty"`
}

func (d disputeRec) snapshot() connector.DisputeSnapshot {
	return connector.DisputeSnapshot{ProviderDisputeID: d.ID, ProviderPaymentID: d.PaymentID, Status: connector.DisputeStatus(d.Status),
		RawStatus: d.Status, Phase: "chargeback", Reason: d.Reason, Amount: d.Amount, Currency: d.Currency, DueBy: d.DueBy, CreatedAt: d.CreatedAt}
}

func (c *conn) putDispute(ctx context.Context, d *disputeRec) error {
	return c.save(ctx, "dsp:"+d.ID, d)
}

// refreshDispute applies a due ruling. Callers hold mu.
func (c *conn) refreshDispute(ctx context.Context, d *disputeRec) (bool, error) {
	if d.Status != string(connector.DisputeUnderReview) || d.WinAt == 0 || c.ms() < d.WinAt {
		return false, nil
	}
	d.Status, d.WinAt = string(connector.DisputeWon), 0
	return true, c.putDispute(ctx, d)
}

func (c *conn) getDispute(ctx context.Context, id string) (*disputeRec, error) {
	var d disputeRec
	ok, err := c.load(ctx, "dsp:"+id, &d)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connector.ErrNotFound
	}
	if _, err := c.refreshDispute(ctx, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// SimulateDispute opens a dispute for the whole amount of a captured payment
// and sends the dispute.created webhook.
func (c *conn) SimulateDispute(ctx context.Context, providerPaymentID, reason string) (connector.DisputeSnapshot, error) {
	if reason == "" {
		reason = "fraudulent"
	}
	mu.Lock()
	p, err := c.findPay(ctx, connector.PaymentRef{ProviderPaymentID: providerPaymentID})
	if err != nil {
		mu.Unlock()
		return connector.DisputeSnapshot{}, err
	}
	if p.Status != stCaptured {
		mu.Unlock()
		return connector.DisputeSnapshot{}, errors.New("simulator: only a captured payment can be disputed")
	}
	now := c.now()
	d := &disputeRec{ID: newID("sim_dsp_"), PaymentID: p.PaymentID, Amount: p.Amount, Currency: p.Currency, Status: string(connector.DisputeNeedsResponse),
		Reason: reason, DueBy: now.Add(7 * 24 * time.Hour).UnixMilli(), CreatedAt: now.UnixMilli()}
	if err := c.putDispute(ctx, d); err != nil {
		mu.Unlock()
		return connector.DisputeSnapshot{}, err
	}
	if err := c.appendList(ctx, "list:dispute", d.ID); err != nil {
		mu.Unlock()
		return connector.DisputeSnapshot{}, err
	}
	snap := d.snapshot()
	mu.Unlock()
	c.emit(event{Type: "dispute.created", Dispute: &snap})
	return snap, nil
}

func (c *conn) ListDisputes(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	mu.Lock()
	defer mu.Unlock()
	ids, err := c.list(ctx, "list:dispute")
	if err != nil {
		return connector.Page[connector.DisputeSnapshot]{}, err
	}
	var all []connector.DisputeSnapshot
	for _, id := range ids {
		d, err := c.getDispute(ctx, id)
		if err != nil {
			continue
		}
		if d.CreatedAt >= tr.From && (tr.To == 0 || d.CreatedAt < tr.To) {
			all = append(all, d.snapshot())
		}
	}
	return page(all, cur), nil
}

func (c *conn) FetchDispute(ctx context.Context, ref connector.DisputeRef) (connector.DisputeSnapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	d, err := c.getDispute(ctx, ref.ProviderDisputeID)
	if err != nil {
		return connector.DisputeSnapshot{}, err
	}
	return d.snapshot(), nil
}

// AcceptDispute concedes it.
func (c *conn) AcceptDispute(ctx context.Context, ref connector.DisputeRef) error {
	mu.Lock()
	d, err := c.getDispute(ctx, ref.ProviderDisputeID)
	if err != nil {
		mu.Unlock()
		return err
	}
	if d.Status != string(connector.DisputeNeedsResponse) {
		mu.Unlock()
		return errors.New("simulator: the dispute is not waiting for a response")
	}
	d.Status = string(connector.DisputeLost)
	err = c.putDispute(ctx, d)
	snap := d.snapshot()
	mu.Unlock()
	if err == nil {
		c.emit(event{Type: "dispute.lost", Dispute: &snap})
	}
	return err
}

// SubmitEvidence puts the dispute under review; a few seconds later the
// simulated bank rules for the merchant.
func (c *conn) SubmitEvidence(ctx context.Context, ref connector.DisputeRef, ev connector.Evidence) error {
	if ev.Text == "" && len(ev.Files) == 0 {
		return errors.New("simulator: evidence needs a summary or a file")
	}
	mu.Lock()
	d, err := c.getDispute(ctx, ref.ProviderDisputeID)
	if err != nil {
		mu.Unlock()
		return err
	}
	if d.Status != string(connector.DisputeNeedsResponse) {
		mu.Unlock()
		return errors.New("simulator: the dispute is not waiting for a response")
	}
	d.Status, d.WinAt = string(connector.DisputeUnderReview), c.ms()+evidenceReviewDelay.Milliseconds()
	err = c.putDispute(ctx, d)
	snap := d.snapshot()
	id := d.ID
	mu.Unlock()
	if err != nil {
		return err
	}
	c.emit(event{Type: "dispute.under_review", Dispute: &snap})
	time.AfterFunc(evidenceReviewDelay+50*time.Millisecond, func() { c.rule(id) })
	return nil
}

// rule delivers the ruling of an under-review dispute.
func (c *conn) rule(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mu.Lock()
	var d disputeRec
	ok, err := c.load(ctx, "dsp:"+id, &d)
	changed := false
	if err == nil && ok {
		changed, err = c.refreshDispute(ctx, &d)
	}
	mu.Unlock()
	if err != nil || !changed {
		return
	}
	snap := d.snapshot()
	c.emit(event{Type: "dispute.won", Dispute: &snap})
}
