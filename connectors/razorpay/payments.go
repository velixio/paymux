package razorpay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
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
		if err := r.api.do(ctx, "GET", "/v1/payments/"+url.PathEscape(ref.ProviderPaymentID), nil, nil, &p, nil); err != nil {
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
			if len(c.Items) == 0 {
				return connector.PaymentSnapshot{}, connector.ErrNotFound
			}
			orderID = c.Items[0].ID
		}
		return r.fetchOrder(ctx, orderID, ref.ProviderRef)
	}
	return connector.PaymentSnapshot{}, errors.New("razorpay: PaymentRef has no id")
}

func (r *razorpay) fetchOrder(ctx context.Context, orderID, providerRef string) (connector.PaymentSnapshot, error) {
	var o orderEntity
	if err := r.api.do(ctx, "GET", "/v1/orders/"+url.PathEscape(orderID), nil, nil, &o, nil); err != nil {
		return connector.PaymentSnapshot{}, err
	}
	var c collection[paymentEntity]
	if err := r.api.do(ctx, "GET", "/v1/orders/"+url.PathEscape(orderID)+"/payments", nil, nil, &c, nil); err != nil {
		return connector.PaymentSnapshot{}, err
	}
	if len(c.Items) == 0 {
		return connector.PaymentSnapshot{
			ProviderRef: providerRef, ProviderOrderID: o.ID, Status: connector.PaymentRequiresAction,
			RawStatus: o.Status, Amount: o.Amount, Currency: o.Currency,
		}, nil
	}
	best := c.Items[0]
	for _, p := range c.Items[1:] {
		if rank(p) > rank(best) || (rank(p) == rank(best) && p.CreatedAt > best.CreatedAt) {
			best = p
		}
	}
	if best.OrderID == "" {
		best.OrderID = orderID
	}
	return r.captureIfAuthorized(ctx, best, providerRef)
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
		case errors.As(err, &ae) && ae.Status == 400 && strings.Contains(strings.ToLower(ae.Description), "already"):
			// Captured concurrently (or by auto-capture): read the current state.
			if err := r.api.do(ctx, "GET", "/v1/payments/"+url.PathEscape(p.ID), nil, nil, &p, nil); err != nil {
				return connector.PaymentSnapshot{}, err
			}
		default:
			return connector.PaymentSnapshot{}, fmt.Errorf("razorpay: capturing %s: %w", p.ID, err)
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
