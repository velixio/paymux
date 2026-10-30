package razorpay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/velixio/paymux/pkg/connector"
)

const listPageSize = 100

type collection[T any] struct {
	Items []T `json:"items"`
}

type orderEntity struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Currency string `json:"currency"`
	Receipt  string `json:"receipt"`
	Amount   int64  `json:"amount"`
}

// FetchPayment returns the provider's view of a payment.
//
// An order may hold several payment attempts (failed ones, then a good one);
// the best one wins: captured/refunded, then authorized, then created, then
// the latest failure. An order without payments reports requires_action.
//
// CAPTURE: if the chosen payment is "authorized" (merchant account has manual
// capture), FetchPayment captures it for the full amount. Razorpay
// auto-refunds uncaptured payments after a few days, so a paid customer would
// otherwise lose the sale. The capture is safe to repeat: if Razorpay answers
// that the payment is already captured we re-read it instead of failing.
func (r *razorpay) FetchPayment(ctx context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	switch {
	case ref.ProviderPaymentID != "":
		var p paymentEntity
		if err := r.api.do(ctx, "GET", "/v1/payments/"+url.PathEscape(ref.ProviderPaymentID), url.Values{"expand[]": {"card"}}, nil, &p, nil); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		// The payment id may have come from outside (a return URL, a
		// notification): use, and above all capture, only a payment of the
		// order or receipt we are asking about.
		if err := r.paymentIsOurs(ctx, p, ref); err != nil {
			return connector.PaymentSnapshot{}, err
		}
		return r.captureIfAuthorized(ctx, p, ref.ProviderRef)
	case ref.ProviderOrderID != "" || ref.ProviderRef != "":
		orderID := ref.ProviderOrderID
		if orderID == "" {
			var c collection[orderEntity]
			q := url.Values{"receipt": {ref.ProviderRef}, "count": {"1"}}
			if err := r.api.do(ctx, "GET", "/v1/orders", q, nil, &c, nil); err != nil {
				return connector.PaymentSnapshot{}, err
			}
			// The filter is Razorpay's to apply; an order of another receipt is
			// not ours whatever the list says.
			for _, o := range c.Items {
				if o.Receipt == ref.ProviderRef {
					orderID = o.ID
					break
				}
			}
			if orderID == "" {
				return connector.PaymentSnapshot{}, connector.ErrNotFound
			}
		}
		return r.fetchOrder(ctx, orderID, ref.ProviderRef)
	}
	return connector.PaymentSnapshot{}, errors.New("razorpay: PaymentRef has no id")
}

// paymentIsOurs says whether a payment fetched by id belongs to ref: to its
// order when ref names one, else to an order whose receipt is ref's. With
// neither to compare it accepts the payment (a lookup by payment id alone).
func (r *razorpay) paymentIsOurs(ctx context.Context, p paymentEntity, ref connector.PaymentRef) error {
	notOurs := fmt.Errorf("%w: payment %s is not of the order or receipt asked about", connector.ErrNotFound, p.ID)
	switch {
	case ref.ProviderOrderID != "":
		if p.OrderID != ref.ProviderOrderID {
			return notOurs
		}
	case ref.ProviderRef != "":
		if p.OrderID == "" {
			return notOurs
		}
		var o orderEntity
		if err := r.api.do(ctx, "GET", "/v1/orders/"+url.PathEscape(p.OrderID), nil, nil, &o, nil); err != nil {
			return err
		}
		if o.Receipt != ref.ProviderRef {
			return notOurs
		}
	}
	return nil
}

func (r *razorpay) fetchOrder(ctx context.Context, orderID, providerRef string) (connector.PaymentSnapshot, error) {
	o, payments, err := r.fetchOrderPayments(ctx, orderID, providerRef)
	if err != nil {
		return connector.PaymentSnapshot{}, err
	}
	if len(payments) == 0 {
		return connector.PaymentSnapshot{
			ProviderRef: providerRef, ProviderOrderID: o.ID, Status: connector.PaymentRequiresAction,
			RawStatus: o.Status, Amount: o.Amount, Currency: o.Currency,
		}, nil
	}
	best := payments[0]
	for _, p := range payments[1:] {
		if rank(p) > rank(best) || (rank(p) == rank(best) && p.CreatedAt > best.CreatedAt) {
			best = p
		}
	}
	if best.OrderID == "" {
		best.OrderID = orderID
	}
	return r.captureIfAuthorized(ctx, best, providerRef)
}

// FetchPaymentAttempts returns every Razorpay payment made against an order.
// Paymux uses it for operator-visible history; FetchPayment still chooses and
// applies the single authoritative payment.
func (r *razorpay) FetchPaymentAttempts(ctx context.Context, ref connector.PaymentRef) ([]connector.PaymentSnapshot, error) {
	if ref.ProviderOrderID == "" {
		return nil, errors.New("razorpay: payment attempt history requires an order id")
	}
	_, payments, err := r.fetchOrderPayments(ctx, ref.ProviderOrderID, ref.ProviderRef)
	if err != nil {
		return nil, err
	}
	out := make([]connector.PaymentSnapshot, 0, len(payments))
	for _, p := range payments {
		if p.OrderID == "" {
			p.OrderID = ref.ProviderOrderID
		}
		snap := p.snapshot()
		snap.ProviderRef = ref.ProviderRef
		out = append(out, snap)
	}
	return out, nil
}

func (r *razorpay) fetchOrderPayments(ctx context.Context, orderID, providerRef string) (orderEntity, []paymentEntity, error) {
	var o orderEntity
	if err := r.api.do(ctx, "GET", "/v1/orders/"+url.PathEscape(orderID), nil, nil, &o, nil); err != nil {
		return orderEntity{}, nil, err
	}
	// The order must be the one made for this reference (its receipt), or the
	// caller's reference would be stamped onto someone else's payment.
	if providerRef != "" && o.Receipt != "" && o.Receipt != providerRef {
		return orderEntity{}, nil, connector.ErrNotFound
	}
	var c collection[paymentEntity]
	if err := r.api.do(ctx, "GET", "/v1/orders/"+url.PathEscape(orderID)+"/payments", url.Values{"expand[]": {"card"}}, nil, &c, nil); err != nil {
		return orderEntity{}, nil, err
	}
	return o, c.Items, nil
}

func rank(p paymentEntity) int {
	switch p.Status {
	case "captured", "refunded":
		return 4
	case "authorized":
		return 3
	case "created":
		return 2
	}
	return 1
}

func (r *razorpay) captureIfAuthorized(ctx context.Context, p paymentEntity, providerRef string) (connector.PaymentSnapshot, error) {
	if p.Status == "authorized" {
		priorCard, priorUPI := p.Card, p.UPI
		priorBank, priorWallet, priorMethod := p.Bank, p.Wallet, p.Method
		var captured paymentEntity
		body := map[string]any{"amount": p.Amount, "currency": p.Currency}
		err := r.api.do(ctx, "POST", "/v1/payments/"+url.PathEscape(p.ID)+"/capture", nil, body, &captured, nil)
		var ae *apiError
		switch {
		case err == nil:
			if captured.OrderID == "" {
				captured.OrderID = p.OrderID
			}
			p = captured
			if p.Card == nil {
				p.Card = priorCard
			}
			if p.UPI == nil {
				p.UPI = priorUPI
			}
			if p.Bank == "" {
				p.Bank = priorBank
			}
			if p.Wallet == "" {
				p.Wallet = priorWallet
			}
			if p.Method == "" {
				p.Method = priorMethod
			}
		case errors.As(err, &ae) && ae.Status == 400 && strings.Contains(strings.ToLower(ae.Description), "already"):
			// Captured concurrently (or by auto-capture): read the current state.
			if err := r.api.do(ctx, "GET", "/v1/payments/"+url.PathEscape(p.ID), nil, nil, &p, nil); err != nil {
				return connector.PaymentSnapshot{}, err
			}
		default:
			return connector.PaymentSnapshot{}, fmt.Errorf("razorpay: capturing %s: %w", p.ID, err)
		}
	}
	if p.Method == "card" && p.Card == nil {
		if err := r.api.do(ctx, "GET", "/v1/payments/"+url.PathEscape(p.ID), url.Values{"expand[]": {"card"}}, nil, &p, nil); err != nil {
			// Card expansion is optional enrichment. Never lose an already fetched
			// captured state merely because that follow-up request failed.
			s := p.snapshot()
			s.ProviderRef = providerRef
			return s, nil
		}
	}
	s := p.snapshot()
	s.ProviderRef = providerRef
	return s, nil
}

// Refund creates a refund. The Paymux refund provider_ref is sent as the
// receipt and as the X-Refund-Idempotency header, so a retried job returns the
// same refund instead of creating a second one.
func (r *razorpay) Refund(ctx context.Context, req connector.RefundRequest) (connector.RefundSnapshot, error) {
	if req.ProviderPaymentID == "" || req.Amount <= 0 {
		return connector.RefundSnapshot{}, errors.New("razorpay: refund needs a payment id and a positive amount")
	}
	if minimum := info.MinRefundMinor[strings.ToUpper(req.Currency)]; minimum > 0 && req.Amount < minimum {
		return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: fmt.Sprintf("Razorpay requires a minimum refund of %d minor units for %s", minimum, strings.ToUpper(req.Currency))}
	}
	body := map[string]any{"amount": req.Amount, "receipt": req.ProviderRef}
	if req.Reason != "" {
		body["notes"] = map[string]string{"reason": req.Reason}
	}
	var e refundEntity
	err := r.api.do(ctx, "POST", "/v1/payments/"+url.PathEscape(req.ProviderPaymentID)+"/refund", nil, body, &e,
		map[string]string{"X-Refund-Idempotency": req.ProviderRef})
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 400 {
			return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: ae.Description}
		}
		return connector.RefundSnapshot{}, err
	}
	s := e.snapshot()
	s.ProviderRef = req.ProviderRef
	return s, nil
}

// FetchRefund looks a refund up by provider id, or by receipt within a payment.
func (r *razorpay) FetchRefund(ctx context.Context, ref connector.RefundRef) (connector.RefundSnapshot, error) {
	if ref.ProviderRefundID != "" {
		var e refundEntity
		if err := r.api.do(ctx, "GET", "/v1/refunds/"+url.PathEscape(ref.ProviderRefundID), nil, nil, &e, nil); err != nil {
			// The sandbox answers unknown refund ids with a bare 400
			// "invalid request sent"; for a single-resource GET that means not found.
			var ae *apiError
			if errors.As(err, &ae) && ae.Status == 400 && !errors.Is(err, connector.ErrNotFound) {
				return connector.RefundSnapshot{}, fmt.Errorf("%w: %w", connector.ErrNotFound, err)
			}
			return connector.RefundSnapshot{}, err
		}
		return e.snapshot(), nil
	}
	if ref.ProviderRef == "" || ref.ProviderPaymentID == "" {
		return connector.RefundSnapshot{}, errors.New("razorpay: RefundRef needs a refund id, or a receipt and a payment id")
	}
	var c collection[refundEntity]
	if err := r.api.do(ctx, "GET", "/v1/payments/"+url.PathEscape(ref.ProviderPaymentID)+"/refunds", nil, nil, &c, nil); err != nil {
		return connector.RefundSnapshot{}, err
	}
	for _, e := range c.Items {
		if e.Receipt == ref.ProviderRef {
			return e.snapshot(), nil
		}
	}
	return connector.RefundSnapshot{}, connector.ErrNotFound
}

// listQuery builds the from/to/count/skip query. Razorpay takes unix seconds
// and an inclusive "to"; the SDK range is half-open milliseconds.
func listQuery(tr connector.TimeRange, cur connector.Cursor) (url.Values, int, error) {
	skip := 0
	if cur != "" {
		n, err := strconv.Atoi(string(cur))
		if err != nil || n < 0 {
			return nil, 0, fmt.Errorf("razorpay: bad cursor %q", cur)
		}
		skip = n
	}
	q := url.Values{"count": {strconv.Itoa(listPageSize)}, "skip": {strconv.Itoa(skip)}}
	if tr.From > 0 {
		q.Set("from", strconv.FormatInt(tr.From/1000, 10))
	}
	if tr.To > 0 {
		q.Set("to", strconv.FormatInt((tr.To-1)/1000, 10))
	}
	return q, skip, nil
}

func nextCursor(n, skip int) connector.Cursor {
	if n < listPageSize {
		return ""
	}
	return connector.Cursor(strconv.Itoa(skip + n))
}

// ListPayments lists payments created in the range, one page at a time.
func (r *razorpay) ListPayments(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.PaymentSnapshot], error) {
	q, skip, err := listQuery(tr, cur)
	if err != nil {
		return connector.Page[connector.PaymentSnapshot]{}, err
	}
	q.Add("expand[]", "card")
	var c collection[paymentEntity]
	if err := r.api.do(ctx, "GET", "/v1/payments", q, nil, &c, nil); err != nil {
		return connector.Page[connector.PaymentSnapshot]{}, err
	}
	pg := connector.Page[connector.PaymentSnapshot]{Items: make([]connector.PaymentSnapshot, 0, len(c.Items)), Next: nextCursor(len(c.Items), skip)}
	for _, p := range c.Items {
		pg.Items = append(pg.Items, p.snapshot())
	}
	return pg, nil
}

// ListRefunds lists refunds created in the range, one page at a time.
func (r *razorpay) ListRefunds(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.RefundSnapshot], error) {
	q, skip, err := listQuery(tr, cur)
	if err != nil {
		return connector.Page[connector.RefundSnapshot]{}, err
	}
	var c collection[refundEntity]
	if err := r.api.do(ctx, "GET", "/v1/refunds", q, nil, &c, nil); err != nil {
		return connector.Page[connector.RefundSnapshot]{}, err
	}
	pg := connector.Page[connector.RefundSnapshot]{Items: make([]connector.RefundSnapshot, 0, len(c.Items)), Next: nextCursor(len(c.Items), skip)}
	for _, e := range c.Items {
		pg.Items = append(pg.Items, e.snapshot())
	}
	return pg, nil
}

// ListDisputes lists disputes created in the range.
func (r *razorpay) ListDisputes(ctx context.Context, tr connector.TimeRange, cur connector.Cursor) (connector.Page[connector.DisputeSnapshot], error) {
	q, skip, err := listQuery(tr, cur)
	if err != nil {
		return connector.Page[connector.DisputeSnapshot]{}, err
	}
	var c collection[disputeEntity]
	if err := r.api.do(ctx, "GET", "/v1/disputes", q, nil, &c, nil); err != nil {
		return connector.Page[connector.DisputeSnapshot]{}, err
	}
	pg := connector.Page[connector.DisputeSnapshot]{Items: make([]connector.DisputeSnapshot, 0, len(c.Items)), Next: nextCursor(len(c.Items), skip)}
	for _, d := range c.Items {
		pg.Items = append(pg.Items, d.snapshot())
	}
	return pg, nil
}

// FetchDispute returns one dispute.
func (r *razorpay) FetchDispute(ctx context.Context, ref connector.DisputeRef) (connector.DisputeSnapshot, error) {
	var d disputeEntity
	if err := r.api.do(ctx, "GET", "/v1/disputes/"+url.PathEscape(ref.ProviderDisputeID), nil, nil, &d, nil); err != nil {
		return connector.DisputeSnapshot{}, err
	}
	return d.snapshot(), nil
}

var (
	_ connector.Refunder      = (*razorpay)(nil)
	_ connector.Lister        = (*razorpay)(nil)
	_ connector.DisputeReader = (*razorpay)(nil)
)
