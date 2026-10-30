package simulator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// poRec is one simulated payout. The beneficiary is deliberately not kept: the
// simulator needs none of it, and it is personal data.
type poRec struct {
	ID        string `json:"id"`
	Ref       string `json:"ref"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Status    string `json:"status"` // processing | processed | failed | reversed
	ErrorCode string `json:"error_code,omitempty"`
	SettleAt  int64  `json:"settle_at,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func (r poRec) snapshot() connector.PayoutSnapshot {
	s := connector.PayoutSnapshot{ProviderRef: r.Ref, ProviderPayoutID: r.ID, RawStatus: r.Status, Amount: r.Amount,
		Currency: r.Currency, ErrorCode: r.ErrorCode, CreatedAt: r.CreatedAt}
	switch r.Status {
	case "processed":
		s.Status = connector.PayoutSucceeded
	case "failed":
		s.Status = connector.PayoutFailed
		s.ErrorMessage = "the simulated provider rejected the payout"
	case "reversed":
		s.Status = connector.PayoutReversed
	default:
		s.Status = connector.PayoutProcessing
	}
	return s
}

func (r poRec) eventType() string {
	switch r.Status {
	case "processed":
		return "payout.processed"
	case "failed":
		return "payout.failed"
	case "reversed":
		return "payout.reversed"
	}
	return "payout.created"
}

func (c *conn) putPayout(ctx context.Context, r *poRec) error {
	if err := c.save(ctx, "po:"+r.ID, r); err != nil {
		return err
	}
	return c.saveString(ctx, "ixpayout:"+r.Ref, r.ID)
}

func (c *conn) findPayout(ctx context.Context, ref connector.PayoutRef) (*poRec, error) {
	id := ref.ProviderPayoutID
	if id == "" && ref.ProviderRef != "" {
		s, ok, err := c.loadString(ctx, "ixpayout:"+ref.ProviderRef)
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
	var r poRec
	ok, err := c.load(ctx, "po:"+id, &r)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, connector.ErrNotFound
	}
	return &r, nil
}

// settlePayout pays a processing payout whose time has come. Callers hold mu.
func (c *conn) settlePayout(ctx context.Context, r *poRec) (bool, error) {
	if r.Status != "processing" || c.ms() < r.SettleAt {
		return false, nil
	}
	r.Status, r.SettleAt = "processed", 0
	return true, c.putPayout(ctx, r)
}

// CreatePayout is idempotent on ProviderRef. With payout_delay_seconds 0 the
// payout is paid at once; otherwise it is processing and paid after that many
// seconds, unless the dashboard decides first (SimulatePayout).
func (c *conn) CreatePayout(ctx context.Context, req connector.PayoutRequest) (connector.PayoutSnapshot, error) {
	switch {
	case strings.TrimSpace(req.ProviderRef) == "":
		return connector.PayoutSnapshot{}, errors.New("simulator: a payout needs a reference")
	case req.Amount <= 0:
		return connector.PayoutSnapshot{}, errors.New("simulator: a payout needs a positive amount")
	case strings.TrimSpace(req.Currency) == "":
		return connector.PayoutSnapshot{}, errors.New("simulator: a payout needs a currency")
	}
	mu.Lock()
	if prior, err := c.findPayout(ctx, connector.PayoutRef{ProviderRef: req.ProviderRef}); err == nil {
		_, _ = c.settlePayout(ctx, prior)
		mu.Unlock()
		return prior.snapshot(), nil
	}
	delay := time.Duration(c.cfg.PayoutDelaySeconds) * time.Second
	r := &poRec{ID: newID("sim_po_"), Ref: req.ProviderRef, Amount: req.Amount, Currency: strings.ToUpper(req.Currency),
		Status: "processing", SettleAt: c.ms() + delay.Milliseconds(), CreatedAt: c.ms()}
	if err := c.putPayout(ctx, r); err != nil {
		mu.Unlock()
		return connector.PayoutSnapshot{}, err
	}
	if err := c.appendList(ctx, "list:payout", r.ID); err != nil {
		mu.Unlock()
		return connector.PayoutSnapshot{}, err
	}
	var sent bool
	if delay == 0 {
		var err error
		if sent, err = c.settlePayout(ctx, r); err != nil {
			mu.Unlock()
			return connector.PayoutSnapshot{}, err
		}
	}
	snap := r.snapshot()
	mu.Unlock()
	if sent {
		c.emit(event{Type: r.eventType(), Payout: &snap})
	}
	if delay > 0 {
		time.AfterFunc(delay+50*time.Millisecond, func() { c.finishPayout(r.ID) })
	}
	return snap, nil
}

func (c *conn) finishPayout(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	mu.Lock()
	var r poRec
	ok, err := c.load(ctx, "po:"+id, &r)
	changed := false
	if err == nil && ok {
		changed, err = c.settlePayout(ctx, &r)
	}
	mu.Unlock()
	if err != nil {
		c.log.Warn("simulator: finishing a payout", "err", err)
		return
	}
	if changed {
		s := r.snapshot()
		c.emit(event{Type: r.eventType(), Payout: &s})
	}
}

func (c *conn) FetchPayout(ctx context.Context, ref connector.PayoutRef) (connector.PayoutSnapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	r, err := c.findPayout(ctx, ref)
	if err != nil {
		return connector.PayoutSnapshot{}, err
	}
	if _, err := c.settlePayout(ctx, r); err != nil {
		return connector.PayoutSnapshot{}, err
	}
	return r.snapshot(), nil
}

// PayoutOutcome is how the dashboard ends a simulated payout.
type PayoutOutcome string

// The outcomes.
const (
	PayoutSucceed PayoutOutcome = "succeed" // a processing payout is paid
	PayoutFail    PayoutOutcome = "fail"    // a processing payout is rejected
	PayoutReverse PayoutOutcome = "reverse" // a paid payout comes back
)

// SimulatePayout ends the payout with the given reference the way the provider
// would, and sends the matching webhook.
func (c *conn) SimulatePayout(ctx context.Context, providerRef string, outcome PayoutOutcome) (connector.PayoutSnapshot, error) {
	mu.Lock()
	r, err := c.findPayout(ctx, connector.PayoutRef{ProviderRef: providerRef})
	if err != nil {
		mu.Unlock()
		return connector.PayoutSnapshot{}, err
	}
	next, from := "", ""
	switch outcome {
	case PayoutSucceed:
		next, from = "processed", "processing"
	case PayoutFail:
		next, from = "failed", "processing"
		r.ErrorCode = "simulated_failure"
	case PayoutReverse:
		next, from = "reversed", "processed"
	default:
		mu.Unlock()
		return connector.PayoutSnapshot{}, fmt.Errorf("simulator: unknown payout outcome %q", outcome)
	}
	if r.Status != from {
		status := r.Status
		mu.Unlock()
		return connector.PayoutSnapshot{}, fmt.Errorf("simulator: the payout is %s; only a %s payout can be made %s", status, from, next)
	}
	r.Status, r.SettleAt = next, 0
	if err := c.putPayout(ctx, r); err != nil {
		mu.Unlock()
		return connector.PayoutSnapshot{}, err
	}
	snap := r.snapshot()
	mu.Unlock()
	c.emit(event{Type: r.eventType(), Payout: &snap})
	return snap, nil
}

var _ connector.Payouter = (*conn)(nil)
