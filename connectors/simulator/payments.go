package simulator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/velixio/paymux/pkg/connector"
)

// Errors a caller can tell apart.
var (
	// ErrAlreadyDecided: the action does not apply to the payment's state.
	ErrAlreadyDecided = errors.New("simulator: the payment is not in a state that action applies to")
	// ErrUnknownAction: not one of the Action values.
	ErrUnknownAction = errors.New("simulator: unknown action")
)

// Payment record kinds.
const (
	kindPayment  = "payment"       // a one-time checkout
	kindMandate  = "mandate_setup" // the registration debit of a mandate
	kindSubFirst = "sub_checkout"  // the authorisation of a managed subscription
	kindCharge   = "charge"        // a charge with no checkout: mandate debit or subscription renewal
)

// Payment states of the simulated provider.
const (
	stCreated    = "created"
	stPending    = "pending"
	stAuthorized = "authorized"
	stCaptured   = "captured"
	stFailed     = "failed"
	stCanceled   = "canceled"
)

// payRec is one simulated payment.
type payRec struct {
	Key       string `json:"key"`
	Kind      string `json:"kind"`
	Ref       string `json:"ref,omitempty"`
	OrderID   string `json:"order_id,omitempty"`
	PaymentID string `json:"payment_id,omitempty"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Status    string `json:"status"`
	Method    string `json:"method,omitempty"`
	ErrCode   string `json:"err_code,omitempty"`
	ErrMsg    string `json:"err_msg,omitempty"`
	Refunded  int64  `json:"refunded,omitempty"`
	SubID     string `json:"sub_id,omitempty"`
	MandateID string `json:"mandate_id,omitempty"`
	// SettleAt and Next make a pending payment turn into Next at that time.
	SettleAt  int64  `json:"settle_at,omitempty"`
	Next      string `json:"next,omitempty"`
	Late      bool   `json:"late,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (p payRec) fee() (fee, tax int64) {
	fee = (p.Amount*2 + 50) / 100 // 2%
	tax = (fee*18 + 50) / 100     // 18% GST on the fee
	return fee, tax
}

func (p payRec) snapshot() connector.PaymentSnapshot {
	s := connector.PaymentSnapshot{ProviderRef: p.Ref, ProviderOrderID: p.OrderID, ProviderPaymentID: p.PaymentID, SubscriptionID: p.SubID,
		RawStatus: p.Status, Amount: p.Amount, Currency: p.Currency, Method: p.Method, AmountRefunded: p.Refunded,
		ErrorCode: p.ErrCode, ErrorMessage: p.ErrMsg, CreatedAt: p.CreatedAt}
	switch p.Status {
	case stCreated:
		s.Status = connector.PaymentCreated
	case stPending:
		s.Status = connector.PaymentProcessing
	case stAuthorized:
		s.Status = connector.PaymentAuthorized
	case stCaptured:
		s.Status = connector.PaymentSucceeded
		s.Fee, s.Tax = p.fee()
	case stFailed:
		s.Status = connector.PaymentFailed
	default:
		s.Status = connector.PaymentCanceled
	}
	return s
}

// ---- storage ----

func (c *conn) putPay(ctx context.Context, p *payRec) error {
	p.UpdatedAt = c.ms()
	if err := c.save(ctx, "pay:"+p.Key, p); err != nil {
		return err
	}
	for prefix, id := range map[string]string{"ixref:": p.Ref, "ixord:": p.OrderID, "ixpay:": p.PaymentID} {
		if id != "" {
			if err := c.saveString(ctx, prefix+id, p.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *conn) newPay(ctx context.Context, p payRec) (*payRec, error) {
	p.Key = newID("pr_")
	p.CreatedAt = c.ms()
	if p.Status == "" {
		p.Status = stCreated
	}
	if err := c.putPay(ctx, &p); err != nil {
		return nil, err
	}
	if err := c.appendList(ctx, "list:pay", p.Key); err != nil {
		return nil, err
	}
	return &p, nil
}

// findPay resolves a PaymentRef to its record. Callers hold mu.
func (c *conn) findPay(ctx context.Context, ref connector.PaymentRef) (*payRec, error) {
	for _, ix := range []struct{ prefix, id string }{{"ixref:", ref.ProviderRef}, {"ixpay:", ref.ProviderPaymentID}, {"ixord:", ref.ProviderOrderID}} {
		if ix.id == "" {
			continue
		}
		key, ok, err := c.loadString(ctx, ix.prefix+ix.id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var p payRec
		if ok, err := c.load(ctx, "pay:"+key, &p); err != nil {
			return nil, err
		} else if ok {
			return &p, nil
		}
	}
	return nil, connector.ErrNotFound
}

// due applies a delayed transition whose time has come. It reports whether the
// record changed.
func (p *payRec) due(now int64) bool {
	if p.Status != stPending || p.Next == "" || now < p.SettleAt {
		return false
	}
	p.Status, p.Next, p.SettleAt = p.Next, "", 0
	return true
}

// ---- Connector ----

func (c *conn) CreateCheckout(ctx context.Context, req connector.CheckoutRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || req.Amount <= 0 {
		return connector.CheckoutResult{}, errors.New("simulator: a checkout needs a provider_ref and a positive amount")
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
	if existing, err := c.findPay(ctx, connector.PaymentRef{ProviderRef: req.ProviderRef}); err == nil {
		return connector.CheckoutResult{ProviderOrderID: existing.OrderID, NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: page}}, nil
	}
	p, err := c.newPay(ctx, payRec{Kind: kindPayment, Ref: req.ProviderRef, OrderID: "sim_ord_" + req.ProviderRef, Amount: req.Amount, Currency: req.Currency})
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	return connector.CheckoutResult{ProviderOrderID: p.OrderID, NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: page}}, nil
}

func supports(cur string) bool {
	for _, c := range info.Currencies {
		if c == strings.ToUpper(cur) {
			return true
		}
	}
	return false
}

// HandleReturn has nothing to prove: the customer's choice was already
// recorded by the hosted page (Decide). It only names the payment.
func (c *conn) HandleReturn(ctx context.Context, req connector.ReturnRequest) (connector.ReturnResult, error) {
	mu.Lock()
	defer mu.Unlock()
	p, err := c.findPay(ctx, connector.PaymentRef{ProviderRef: req.ProviderRef})
	if err != nil {
		return connector.ReturnResult{}, connector.ErrInvalidReturn
	}
	return connector.ReturnResult{ProviderRef: p.Ref, ProviderOrderID: p.OrderID, ProviderPaymentID: p.PaymentID}, nil
}

func (c *conn) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	p, err := c.findPay(ctx, ref)
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	if p.due(c.ms()) {
		if _, err := c.afterCapture(ctx, p, false); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		if err := c.putPay(ctx, p); err != nil {
			return connector.PaymentSnapshot{}, err
		}
	}
	return p.snapshot(), nil
}

func (c *conn) CancelCheckout(ctx context.Context, ref connector.PaymentRef) error {
	mu.Lock()
	defer mu.Unlock()
	p, err := c.findPay(ctx, ref)
	if err != nil {
		return err
	}
	if p.Status == stCreated || p.Status == stPending {
		p.Status, p.Next, p.SettleAt = stCanceled, "", 0
		return c.putPay(ctx, p)
	}
	return nil
}

func (c *conn) ListPayments(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.PaymentSnapshot], error) {
	mu.Lock()
	defer mu.Unlock()
	keys, err := c.list(ctx, "list:pay")
	if err != nil {
		return connector.Page[connector.PaymentSnapshot]{}, err
	}
	var all []connector.PaymentSnapshot
	for _, k := range keys {
		var p payRec
		if ok, err := c.load(ctx, "pay:"+k, &p); err != nil || !ok {
			continue
		}
		if p.Kind == kindSubFirst || p.PaymentID == "" && p.Status == stCreated {
			continue // a checkout nobody has paid is not in the provider's payment list yet
		}
		if p.due(c.ms()) {
			_, _ = c.afterCapture(ctx, &p, false)
			_ = c.putPay(ctx, &p)
		}
		if p.CreatedAt >= tr.From && (tr.To == 0 || p.CreatedAt < tr.To) {
			all = append(all, p.snapshot())
		}
	}
	return page(all, cur), nil
}

// page returns one page of items, 100 at a time; the cursor is an offset.
func page[T any](all []T, cur connector.Cursor) connector.Page[T] {
	const size = 100
	off := 0
	fmt.Sscanf(string(cur), "%d", &off)
	if off > len(all) {
		off = len(all)
	}
	end := min(off+size, len(all))
	out := connector.Page[T]{Items: all[off:end]}
	if end < len(all) {
		out.Next = connector.Cursor(fmt.Sprint(end))
	}
	return out
}

// ---- the hosted page's side: Controller ----

// Action is what a customer chooses on the hosted page.
type Action string

// The hosted page's buttons.
const (
	ActionSucceed        Action = "succeed"         // capture now
	ActionFail           Action = "fail"            // the payment fails
	ActionPendingSucceed Action = "pending_succeed" // pending, then success after the delay
	ActionAuthorize      Action = "authorize"       // authorised only; capture later
	ActionLateSucceed    Action = "late_succeed"    // success long after: lets the payment expire first
	ActionCapture        Action = "capture"         // capture an authorised payment
	ActionCancel         Action = "cancel"          // the customer abandons
)

// CheckoutState is what the hosted page shows.
type CheckoutState struct {
	Ref      string `json:"ref"`
	Kind     string `json:"kind"` // payment, mandate_setup or sub_checkout
	Status   string `json:"status"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Late     bool   `json:"late,omitempty"`
}

// Controller is the part of the simulated provider that its hosted page and
// the dashboard drive. The connector instance implements it; Paymux reaches it
// by a type assertion on the live connector, which only exists in test mode.
type Controller interface {
	State(ctx context.Context, ref string) (CheckoutState, error)
	// Decide applies the customer's choice. delay is used by the delayed actions.
	Decide(ctx context.Context, ref string, a Action, delay time.Duration) error
	// SimulateDispute opens a dispute on a captured payment.
	SimulateDispute(ctx context.Context, providerPaymentID, reason string) (connector.DisputeSnapshot, error)
	// SimulateProviderRefund makes the "provider" refund a captured payment on
	// its own, as if done in its dashboard: Paymux did not ask for it. amount 0
	// refunds what is left. It sends the refund.processed webhook.
	SimulateProviderRefund(ctx context.Context, providerPaymentID string, amount int64) (connector.RefundSnapshot, error)
	// SimulateRenewal makes a provider-managed subscription charge its next
	// period (or fail to, which turns it past due).
	SimulateRenewal(ctx context.Context, providerSubscriptionID string, fail bool) error
}

var _ Controller = (*conn)(nil)

func (c *conn) State(ctx context.Context, ref string) (CheckoutState, error) {
	mu.Lock()
	defer mu.Unlock()
	p, err := c.findPay(ctx, connector.PaymentRef{ProviderRef: ref})
	if err != nil {
		return CheckoutState{}, err
	}
	if p.due(c.ms()) {
		_, _ = c.afterCapture(ctx, p, false)
		_ = c.putPay(ctx, p)
	}
	return CheckoutState{Ref: p.Ref, Kind: p.Kind, Status: p.Status, Amount: p.Amount, Currency: p.Currency, Late: p.Late}, nil
}

func (c *conn) Decide(ctx context.Context, ref string, a Action, delay time.Duration) error {
	mu.Lock()
	p, err := c.findPay(ctx, connector.PaymentRef{ProviderRef: ref})
	if err != nil {
		mu.Unlock()
		return err
	}
	if p.due(c.ms()) {
		if _, err := c.afterCapture(ctx, p, false); err != nil {
			mu.Unlock()
			return err
		}
	}
	events, schedule, err := c.decide(ctx, p, a, delay)
	if err == nil {
		err = c.putPay(ctx, p)
	}
	mu.Unlock()
	if err != nil {
		return err
	}
	for _, e := range events {
		c.emit(e)
	}
	if schedule > 0 {
		c.settleLater(p.Key, schedule)
	}
	return nil
}

// decide changes p as the customer asked and returns the webhooks to send and,
// for delayed actions, how long until the payment settles. Callers hold mu.
func (c *conn) decide(ctx context.Context, p *payRec, a Action, delay time.Duration) (events []event, schedule time.Duration, err error) {
	open := p.Status == stCreated || p.Status == stPending
	switch a {
	case ActionSucceed, ActionPendingSucceed, ActionLateSucceed, ActionAuthorize, ActionFail:
		if !open {
			return nil, 0, fmt.Errorf("%w: it is already %s", ErrAlreadyDecided, p.Status)
		}
	case ActionCapture:
		if p.Status != stAuthorized {
			return nil, 0, fmt.Errorf("%w: only an authorised payment can be captured, this one is %s", ErrAlreadyDecided, p.Status)
		}
	case ActionCancel:
		if !open {
			return nil, 0, fmt.Errorf("%w: it is already %s", ErrAlreadyDecided, p.Status)
		}
	default:
		return nil, 0, fmt.Errorf("%w %q", ErrUnknownAction, a)
	}
	if p.PaymentID == "" && a != ActionCancel {
		p.PaymentID = newID("sim_pay_")
	}
	switch a {
	case ActionSucceed, ActionCapture:
		p.Status, p.Method = stCaptured, "simulated"
		events, err = c.afterCapture(ctx, p, true)
	case ActionFail:
		p.Status, p.Method, p.ErrCode, p.ErrMsg = stFailed, "simulated", "simulated_failure", "The simulator declined this payment."
		events, err = c.afterFailure(ctx, p)
	case ActionAuthorize:
		p.Status, p.Method = stAuthorized, "simulated"
		events = []event{c.payEvent("payment.authorized", *p)}
	case ActionPendingSucceed, ActionLateSucceed:
		if delay < time.Second {
			delay = time.Second
		}
		p.Status, p.Method, p.Next = stPending, "simulated", stCaptured
		p.SettleAt = c.ms() + delay.Milliseconds()
		p.Late = a == ActionLateSucceed
		schedule = delay
	case ActionCancel:
		p.Status = stCanceled
	}
	return events, schedule, err
}

// settleLater makes the pending payment turn into its next state after d, with
// the webhook the provider would send. If the process stops first, a fetch or
// the poller applies the transition (due) instead.
func (c *conn) settleLater(key string, d time.Duration) {
	time.AfterFunc(d+50*time.Millisecond, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		mu.Lock()
		var p payRec
		ok, err := c.load(ctx, "pay:"+key, &p)
		var events []event
		if err == nil && ok && p.due(c.ms()) {
			events, err = c.afterCapture(ctx, &p, true)
			if err == nil {
				err = c.putPay(ctx, &p)
			}
		}
		mu.Unlock()
		if err != nil {
			c.log.Warn("simulator: settling a pending payment", "err", err)
			return
		}
		for _, e := range events {
			c.emit(e)
		}
	})
}
