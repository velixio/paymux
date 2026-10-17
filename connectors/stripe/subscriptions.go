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
			ID    string `json:"id"`
			Price struct {
				UnitAmount int64  `json:"unit_amount"`
				Currency   string `json:"currency"`
				Product    string `json:"product"`
				Recurring  struct {
					Interval      string `json:"interval"`
					IntervalCount int    `json:"interval_count"`
				} `json:"recurring"`
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
// A start less than 48 hours ahead is ignored (Checkout needs trial_end at least
// that far ahead; verified live). A TotalCount is refused: Checkout rejects
// subscription_data[cancel_at], so there is no way to end the subscription.
func (s *stripe) CreateSubscriptionCheckout(ctx context.Context, req connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || len(req.ProviderRef) > 200 {
		return connector.CheckoutResult{}, errors.New("stripe: provider_ref must be 1..200 chars")
	}
	if req.TotalCount > 0 {
		return connector.CheckoutResult{}, fmt.Errorf("%w: Stripe Checkout cannot end a subscription after %d cycles (subscription_data[cancel_at] is rejected); use a plan without a total count", connector.ErrUnsupported, req.TotalCount)
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

// UpdateSubscriptionAmount gives the subscription a new price from its next
// invoice: the item's inline price is replaced by a new one (same product,
// currency and interval, the new amount) with proration_behavior=none, because
// Paymux decides itself whether and how to prorate. The request is idempotent
// per subscription and amount.
//
// Verified live: the price_data update is accepted, the new amount shows on the
// subscription, and repeating it is harmless.
func (s *stripe) UpdateSubscriptionAmount(ctx context.Context, ref connector.SubscriptionRef, amount int64) error {
	if !strings.HasPrefix(ref.ProviderSubscriptionID, "sub_") {
		return connector.ErrNotFound // nothing was bought yet
	}
	if amount <= 0 {
		return errors.New("stripe: amount must be positive")
	}
	path := "/v1/subscriptions/" + url.PathEscape(ref.ProviderSubscriptionID)
	var w subscriptionWire
	if err := s.api.do(ctx, "GET", path, nil, nil, &w, ""); err != nil {
		return err
	}
	if len(w.Items.Data) != 1 {
		return fmt.Errorf("stripe: subscription %s has %d items; Paymux subscriptions have exactly one", ref.ProviderSubscriptionID, len(w.Items.Data))
	}
	item := w.Items.Data[0]
	if item.ID == "" || item.Price.Product == "" || item.Price.Currency == "" || item.Price.Recurring.Interval == "" {
		return fmt.Errorf("stripe: subscription %s has an item without a product, currency or interval", ref.ProviderSubscriptionID)
	}
	minor, err := toStripe(amount, item.Price.Currency)
	if err != nil {
		return err
	}
	count := item.Price.Recurring.IntervalCount
	if count < 1 {
		count = 1
	}
	f := url.Values{}
	f.Set("items[0][id]", item.ID)
	f.Set("items[0][price_data][currency]", strings.ToLower(item.Price.Currency))
	f.Set("items[0][price_data][product]", item.Price.Product)
	f.Set("items[0][price_data][unit_amount]", fmt.Sprint(minor))
	f.Set("items[0][price_data][recurring][interval]", item.Price.Recurring.Interval)
	f.Set("items[0][price_data][recurring][interval_count]", fmt.Sprint(count))
	f.Set("proration_behavior", "none")
	return s.api.do(ctx, "POST", path, nil, f, nil, fmt.Sprintf("subamount:%s:%d", ref.ProviderSubscriptionID, amount))
}

var _ connector.SubscriptionManager = (*stripe)(nil)
