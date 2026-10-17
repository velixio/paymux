package whop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Recurring billing through Whop memberships (provider_managed). Whop is the
// merchant of record: it charges, retries and tells the customer; Paymux
// creates the checkout, mirrors the membership from webhooks and cancels it.
// Renewal payments arrive as payment.succeeded webhooks and are reported as
// subscription notifications carrying the payment.
//
// TODO(spec): none of this could be checked against the live v1 API. The wire
// details to verify first, all in this file or marked below: the inline
// renewal plan of POST /checkout_configurations (and whether to cache real
// plans instead), billing_period in days, trial_period_days, the membership
// statuses and fields, POST /memberships/{id}/cancel and its cancellation_mode,
// and the membership.* event names.

var intervalDays = map[string]int{"day": 1, "week": 7, "month": 30, "year": 365}

type membershipEntity struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Metadata          map[string]string `json:"metadata"`
	RenewalPeriodFrom string            `json:"renewal_period_start"`
	RenewalPeriodTo   string            `json:"renewal_period_end"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
}

// membershipStatus maps Whop's membership status to the SDK's subscription
// statuses; unknown values map to active (the provider keeps billing).
func membershipStatus(raw string) string {
	switch strings.ToLower(raw) {
	case "drafted", "draft", "pending":
		return "incomplete"
	case "past_due", "unresolved":
		return "past_due"
	case "canceled", "cancelled", "expired":
		return "canceled"
	case "completed":
		return "ended"
	}
	return "active" // "trialing", "active" and anything new
}

func (m membershipEntity) snapshot() connector.SubscriptionSnapshot {
	return connector.SubscriptionSnapshot{ProviderSubscriptionID: m.ID, ProviderRef: m.Metadata["ref"], Status: membershipStatus(m.Status),
		RawStatus: m.Status, CurrentPeriodStart: parseTime(m.RenewalPeriodFrom), CurrentPeriodEnd: parseTime(m.RenewalPeriodTo)}
}

// CreateSubscriptionCheckout creates a checkout configuration with an inline
// renewal plan for the recurring price and redirects the customer to its
// purchase_url. The membership appears once the customer buys; the metadata
// ties it (and each of its payments) back to the subscription.
func (w *whop) CreateSubscriptionCheckout(ctx context.Context, req connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	if req.ProviderRef == "" || req.Amount <= 0 || req.IntervalCount < 1 {
		return connector.CheckoutResult{}, errors.New("whop: provider_ref, a positive amount and interval_count >= 1 are required")
	}
	days, ok := intervalDays[req.Interval]
	if !ok {
		return connector.CheckoutResult{}, fmt.Errorf("whop: unsupported interval %q", req.Interval)
	}
	price, err := money.FormatDecimal(req.Amount, req.Currency)
	if err != nil {
		return connector.CheckoutResult{}, fmt.Errorf("whop: %w", err)
	}
	plan := map[string]any{
		"company_id": w.cfg.CompanyID, "plan_type": "renewal", "currency": strings.ToLower(req.Currency),
		"renewal_price": json.Number(price), "initial_price": json.Number(price), "billing_period": days * req.IntervalCount,
		// Whop requires inline product details for a renewal plan. A stable
		// external id per price point lets repeat subscriptions share a product.
		"product": map[string]any{
			"title":               firstNonEmptyStr(req.Description, "Subscription"),
			"external_identifier": fmt.Sprintf("paymux-renewal-%s-%d-%s-%d", strings.ToLower(req.Currency), req.Amount, req.Interval, req.IntervalCount),
		},
	}
	if req.StartAt > 0 {
		if d := time.UnixMilli(req.StartAt).Sub(w.deps.Now()); d > 0 {
			plan["trial_period_days"] = int((d + 24*time.Hour - 1) / (24 * time.Hour)) // a trial: nothing is charged until it ends
			plan["initial_price"] = json.Number("0")
		}
	}
	in := map[string]any{
		"plan":     plan,
		"metadata": map[string]string{"sub": req.SubscriptionID, "ref": req.ProviderRef},
	}
	w.setRedirect(in, req.ProviderRef)
	var out struct {
		ID          string `json:"id"`
		PurchaseURL string `json:"purchase_url"`
	}
	if err := w.do(ctx, "POST", "/checkout_configurations", nil, in, &out, nil); err != nil {
		return connector.CheckoutResult{}, err
	}
	if out.ID == "" || out.PurchaseURL == "" {
		return connector.CheckoutResult{}, errors.New("whop: checkout configuration response lacks id or purchase_url")
	}
	// ProviderOrderID is the checkout configuration: the membership id is not
	// known until the customer buys. Notifications carry the subscription's
	// provider_ref (from the metadata) so the membership can be linked.
	return connector.CheckoutResult{ProviderOrderID: out.ID,
		NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: out.PurchaseURL}}, nil
}

// FetchSubscription reads a membership. Before the customer buys, the only id
// is the checkout configuration's: that is reported as not found.
func (w *whop) FetchSubscription(ctx context.Context, ref connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	if !strings.HasPrefix(ref.ProviderSubscriptionID, "mem_") {
		return connector.SubscriptionSnapshot{}, connector.ErrNotFound
	}
	var m membershipEntity
	if err := w.do(ctx, "GET", "/memberships/"+url.PathEscape(ref.ProviderSubscriptionID), nil, nil, &m, nil); err != nil {
		return connector.SubscriptionSnapshot{}, err
	}
	return m.snapshot(), nil
}

// CancelSubscription cancels a membership now or at the end of the period.
func (w *whop) CancelSubscription(ctx context.Context, ref connector.SubscriptionRef, atPeriodEnd bool) error {
	if !strings.HasPrefix(ref.ProviderSubscriptionID, "mem_") {
		return connector.ErrNotFound // nothing was bought yet
	}
	mode := "immediate"
	if atPeriodEnd {
		mode = "at_period_end"
	}
	return w.do(ctx, "POST", "/memberships/"+url.PathEscape(ref.ProviderSubscriptionID)+"/cancel", nil,
		map[string]string{"cancellation_mode": mode}, nil, nil)
}

// UpdateSubscriptionAmount is not supported: the price belongs to the plan,
// which every member of it shares.
func (w *whop) UpdateSubscriptionAmount(context.Context, connector.SubscriptionRef, int64) error {
	return connector.ErrUnsupported
}

var _ connector.SubscriptionManager = (*whop)(nil)

func firstNonEmptyStr(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
