package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// Recurring billing through Stripe Billing (provider-managed). The price is
// created inline in the Checkout Session (price_data with a recurring
// interval), so no product or price objects are kept. The session's metadata
// and the subscription's own carry our provider_ref, which links the
// subscription Stripe creates after the checkout to the Paymux one. Renewals
// arrive as invoice.paid webhooks and become subscription notifications that
// carry the payment.

type subscriptionWire struct {
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	Metadata           map[string]string `json:"metadata"`
	CurrentPeriodStart int64             `json:"current_period_start"`
	CurrentPeriodEnd   int64             `json:"current_period_end"`
	CancelAtPeriodEnd  bool              `json:"cancel_at_period_end"`
	Items              struct {
		Data []struct {
			Price struct {
				UnitAmount int64  `json:"unit_amount"`
				Currency   string `json:"currency"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// mapSubStatus maps Stripe's subscription status to the SDK's. Unknown values
// map to active: the provider keeps billing until it says otherwise.
func mapSubStatus(raw string) string {
	switch raw {
	case "incomplete":
		return "incomplete"
	case "past_due", "unpaid", "paused":
		return "past_due"
	case "canceled", "incomplete_expired":
		return "canceled"
	}
	return "active" // "active", "trialing" and anything new
}

func (w subscriptionWire) snapshot() connector.SubscriptionSnapshot {
	snap := connector.SubscriptionSnapshot{ProviderSubscriptionID: w.ID, ProviderRef: w.Metadata[metaRef], Status: mapSubStatus(w.Status),
		RawStatus: w.Status, CurrentPeriodStart: ms(w.CurrentPeriodStart), CurrentPeriodEnd: ms(w.CurrentPeriodEnd)}
	if len(w.Items.Data) > 0 {
		p := w.Items.Data[0].Price
		snap.Amount, snap.Currency = fromStripe(p.UnitAmount, p.Currency), upper(p.Currency)
	}
	return snap
}

var validIntervals = map[string]bool{"day": true, "week": true, "month": true, "year": true}

// CreateSubscriptionCheckout creates a Checkout Session in subscription mode
// and redirects the customer to it. ProviderOrderID is the session: the
// subscription itself (sub_...) exists only once the customer has paid.
//
// A StartAt in the future becomes a trial (subscription_data[trial_end]);
// TotalCount becomes cancel_at, counted in whole intervals from the start.
//
// TODO(spec): Checkout requires trial_end to be at least 48 hours ahead and
// cancel_at to be a future timestamp; verify both against the live API.
func (s *stripe) CreateSubscriptionCheckout(ctx context.Context, req connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || len(req.ProviderRef) > 200 {
		return connector.CheckoutResult{}, errors.New("stripe: provider_ref must be 1..200 chars")
	}
	if !validIntervals[req.Interval] || req.IntervalCount < 1 {
		return connector.CheckoutResult{}, fmt.Errorf("stripe: unsupported interval %d x %q", req.IntervalCount, req.Interval)
	}
	amount, err := toStripe(req.Amount, req.Currency)
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	if amount <= 0 {
		return connector.CheckoutResult{}, errors.New("stripe: amount must be positive")
	}
	ret := s.deps.URLs.ReturnURL(req.ProviderRef)
	f := url.Values{}
	f.Set("mode", "subscription")
	f.Set("client_reference_id", req.ProviderRef)
	f.Set("success_url", withQuery(ret, "session_id", "{CHECKOUT_SESSION_ID}"))
	f.Set("cancel_url", withQuery(ret, "stripe_canceled", "1"))
	f.Set("line_items[0][quantity]", "1")
	f.Set("line_items[0][price_data][currency]", strings.ToLower(req.Currency))
	f.Set("line_items[0][price_data][unit_amount]", fmt.Sprint(amount))
	f.Set("line_items[0][price_data][recurring][interval]", req.Interval)
	f.Set("line_items[0][price_data][recurring][interval_count]", fmt.Sprint(req.IntervalCount))
	f.Set("line_items[0][price_data][product_data][name]", productName(req.Description))
	for k, v := range map[string]string{metaRef: req.ProviderRef, metaSub: req.SubscriptionID} {
		if v != "" {
			f.Set("metadata["+k+"]", v)
			f.Set("subscription_data[metadata]["+k+"]", v)
		}
	}
	for k, v := range req.Metadata {
		f.Set("subscription_data[metadata][u_"+k+"]", v)
	}
	start := s.deps.Now()
	if req.StartAt > 0 {
		if at := time.UnixMilli(req.StartAt); at.After(start.Add(48 * time.Hour)) {
			f.Set("subscription_data[trial_end]", fmt.Sprint(at.Unix()))
			start = at
		}
	}
	if req.TotalCount > 0 {
		end := start
		switch req.Interval {
		case "day":
			end = end.AddDate(0, 0, req.IntervalCount*req.TotalCount)
		case "week":
			end = end.AddDate(0, 0, 7*req.IntervalCount*req.TotalCount)
		case "month":
			end = end.AddDate(0, req.IntervalCount*req.TotalCount, 0)
		case "year":
			end = end.AddDate(req.IntervalCount*req.TotalCount, 0, 0)
		}
		// Half a day before the last renewal would be due, so the term ends at the last paid period.
		f.Set("subscription_data[cancel_at]", fmt.Sprint(end.Add(-12*time.Hour).Unix()))
	}
	if req.Customer.Email != "" {
		f.Set("customer_email", req.Customer.Email)
	}
	var sess session
	if err := s.api.do(ctx, "POST", "/v1/checkout/sessions", nil, f, &sess, "subcheckout:"+req.ProviderRef); err != nil {
		return connector.CheckoutResult{}, err
	}
	if sess.ID == "" || !strings.HasPrefix(sess.URL, "https://") {
		return connector.CheckoutResult{}, errors.New("stripe: checkout session response lacks id or an https url")
	}
	return connector.CheckoutResult{ProviderOrderID: sess.ID, ExpiresAt: ms(sess.ExpiresAt),
		NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: sess.URL}}, nil
}

// FetchSubscription reads a subscription. Before the customer has paid the only
// id is the session's, which is not a subscription: not found.
func (s *stripe) FetchSubscription(ctx context.Context, ref connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	if !strings.HasPrefix(ref.ProviderSubscriptionID, "sub_") {
		return connector.SubscriptionSnapshot{}, connector.ErrNotFound
	}
	var w subscriptionWire
	if err := s.api.do(ctx, "GET", "/v1/subscriptions/"+url.PathEscape(ref.ProviderSubscriptionID), nil, nil, &w, ""); err != nil {
		return connector.SubscriptionSnapshot{}, err
	}
	return w.snapshot(), nil
}

// CancelSubscription cancels now (DELETE) or at the end of the period.
func (s *stripe) CancelSubscription(ctx context.Context, ref connector.SubscriptionRef, atPeriodEnd bool) error {
	if !strings.HasPrefix(ref.ProviderSubscriptionID, "sub_") {
		return connector.ErrNotFound // nothing was bought yet
	}
	path := "/v1/subscriptions/" + url.PathEscape(ref.ProviderSubscriptionID)
	if atPeriodEnd {
		return s.api.do(ctx, "POST", path, nil, url.Values{"cancel_at_period_end": {"true"}}, nil, "")
	}
	return s.api.do(ctx, "DELETE", path, nil, nil, nil, "")
}

// UpdateSubscriptionAmount is not supported: the inline price belongs to the
// subscription item, and replacing it mid-cycle needs a proration decision
// Paymux makes itself (it refuses proration for provider-managed billing).
//
// TODO(spec): implement as a new inline price on the subscription item with
// proration_behavior=none when plan changes are needed on Stripe.
func (s *stripe) UpdateSubscriptionAmount(context.Context, connector.SubscriptionRef, int64) error {
	return connector.ErrUnsupported
}

var _ connector.SubscriptionManager = (*stripe)(nil)
