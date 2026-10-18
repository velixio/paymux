package razorpay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/paymux/paymux/pkg/connector"
)

// Razorpay Subscriptions (provider_managed billing). Razorpay owns the
// schedule, pre-debit notices and retries; Paymux creates the subscription,
// mirrors its state from webhooks and cancels or re-prices it on request.
//
// From Razorpay's documentation: total_count times the interval may not exceed
// 100 years (see maxPeriods); PATCH /v1/subscriptions takes schedule_change_at
// "now" or "cycle_end"; a subscription paid by UPI cannot be updated at all.
// Not run against the test API (no secret was available).

type planInfo struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Period   string `json:"period"`
	Interval int    `json:"interval"`
}

var periods = map[string]string{"day": "daily", "week": "weekly", "month": "monthly", "year": "yearly"}

// defaultTotalCount is used when the caller gives no total_count (Razorpay
// requires one); it is roughly "until cancelled" for each period.
// maxPeriods is the most billing periods (times the interval) Razorpay allows:
// a subscription may run 100 years at most.
var maxPeriods = map[string]int{"daily": 36500, "weekly": 5200, "monthly": 1200, "yearly": 100}

var defaultTotalCount = map[string]int{"daily": 3650, "weekly": 520, "monthly": 120, "yearly": 30}

func planKey(cur string, amount int64, period string, interval int) string {
	return fmt.Sprintf("razorpay:plan:%s:%d:%s:%d", cur, amount, period, interval)
}

func planInfoKey(id string) string { return "razorpay:planinfo:" + id }

func (r *razorpay) kvPlanInfo(ctx context.Context, id string) (planInfo, bool) {
	if r.deps.KV == nil {
		return planInfo{}, false
	}
	b, ok, err := r.deps.KV.Get(ctx, planInfoKey(id))
	if err != nil || !ok {
		return planInfo{}, false
	}
	var p planInfo
	if json.Unmarshal(b, &p) != nil {
		return planInfo{}, false
	}
	return p, true
}

func (r *razorpay) storePlan(ctx context.Context, id string, p planInfo) {
	if r.deps.KV == nil {
		return
	}
	b, _ := json.Marshal(p)
	_ = r.deps.KV.Set(ctx, planInfoKey(id), b, 0)
	_ = r.deps.KV.Set(ctx, planKey(p.Currency, p.Amount, p.Period, p.Interval), []byte(id), 0)
}

// planFor returns the provider plan id for (amount, currency, period, interval),
// creating and caching it in KV on first use.
func (r *razorpay) planFor(ctx context.Context, p planInfo) (string, error) {
	key := planKey(p.Currency, p.Amount, p.Period, p.Interval)
	if r.deps.KV != nil {
		if b, ok, err := r.deps.KV.Get(ctx, key); err == nil && ok && len(b) > 0 {
			return string(b), nil
		}
	}
	in := map[string]any{
		"period": p.Period, "interval": p.Interval,
		"item": map[string]any{
			"name":     fmt.Sprintf("Paymux %s %d every %d %s", p.Currency, p.Amount, p.Interval, p.Period),
			"amount":   p.Amount,
			"currency": p.Currency,
		},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := r.api.do(ctx, "POST", "/v1/plans", nil, in, &out, nil); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("razorpay: plan response has no id")
	}
	r.storePlan(ctx, out.ID, p)
	return out.ID, nil
}

// planDetails reads a plan's price and cadence, KV first.
func (r *razorpay) planDetails(ctx context.Context, id string) (planInfo, error) {
	if p, ok := r.kvPlanInfo(ctx, id); ok {
		return p, nil
	}
	var out struct {
		Period   string `json:"period"`
		Interval int    `json:"interval"`
		Item     struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"item"`
	}
	if err := r.api.do(ctx, "GET", "/v1/plans/"+url.PathEscape(id), nil, nil, &out, nil); err != nil {
		return planInfo{}, err
	}
	p := planInfo{Amount: out.Item.Amount, Currency: out.Item.Currency, Period: out.Period, Interval: out.Interval}
	if r.deps.KV != nil {
		b, _ := json.Marshal(p)
		_ = r.deps.KV.Set(ctx, planInfoKey(id), b, 0)
	}
	return p, nil
}

type subEntity struct {
	ID           string `json:"id"`
	PlanID       string `json:"plan_id"`
	Status       string `json:"status"`
	CurrentStart int64  `json:"current_start"`
	CurrentEnd   int64  `json:"current_end"`
}

func mapSubscriptionStatus(raw string) string {
	switch raw {
	case "created", "authenticated":
		return "incomplete"
	case "pending", "halted": // charge failing / retries exhausted: dunning decides
		return "past_due"
	case "paused":
		return "paused"
	case "cancelled":
		return "canceled"
	case "completed":
		return "ended"
	case "expired":
		return "incomplete_expired"
	default: // "active" and anything new
		return "active"
	}
}

func (r *razorpay) subSnapshot(ctx context.Context, s subEntity, withPlan bool) (connector.SubscriptionSnapshot, error) {
	out := connector.SubscriptionSnapshot{
		ProviderSubscriptionID: s.ID, Status: mapSubscriptionStatus(s.Status), RawStatus: s.Status,
		CurrentPeriodStart: ms(s.CurrentStart), CurrentPeriodEnd: ms(s.CurrentEnd),
	}
	if s.PlanID == "" {
		return out, nil
	}
	var p planInfo
	var err error
	if withPlan {
		p, err = r.planDetails(ctx, s.PlanID)
		if err != nil {
			return out, err
		}
	} else {
		p, _ = r.kvPlanInfo(ctx, s.PlanID) // webhooks make no API calls; amount is best effort
	}
	out.Amount, out.Currency = p.Amount, p.Currency
	return out, nil
}

// CreateSubscriptionCheckout creates (or reuses) the plan for the amount and
// cadence, creates the subscription and returns the checkout.js options for it.
func (r *razorpay) CreateSubscriptionCheckout(ctx context.Context, req connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	period, ok := periods[req.Interval]
	if !ok {
		return connector.CheckoutResult{}, fmt.Errorf("razorpay: unsupported interval %q", req.Interval)
	}
	if req.Amount <= 0 || req.IntervalCount < 1 || req.ProviderRef == "" {
		return connector.CheckoutResult{}, errors.New("razorpay: subscription needs provider_ref, a positive amount and interval_count >= 1")
	}
	planID, err := r.planFor(ctx, planInfo{Amount: req.Amount, Currency: req.Currency, Period: period, Interval: req.IntervalCount})
	if err != nil {
		return connector.CheckoutResult{}, err
	}
	total := req.TotalCount
	if total <= 0 {
		total = defaultTotalCount[period]
	}
	if total*req.IntervalCount > maxPeriods[period] {
		return connector.CheckoutResult{}, fmt.Errorf("razorpay: total_count %d x every %d %s runs past Razorpay's 100-year limit", total, req.IntervalCount, req.Interval)
	}
	in := map[string]any{
		"plan_id": planID, "total_count": total, "quantity": 1,
		"customer_notify": 1, // Razorpay sends the pre-debit and charge notices
		"notes":           map[string]string{"sub": req.SubscriptionID, "ref": req.ProviderRef},
	}
	if req.StartAt > 0 {
		in["start_at"] = req.StartAt / 1000
	}
	var sub subEntity
	if err := r.api.do(ctx, "POST", "/v1/subscriptions", nil, in, &sub, nil); err != nil {
		return connector.CheckoutResult{}, err
	}
	if sub.ID == "" {
		return connector.CheckoutResult{}, errors.New("razorpay: subscription response has no id")
	}
	opts := map[string]any{
		"key": r.cfg.KeyID, "subscription_id": sub.ID, "name": r.cfg.Name,
		"callback_url": r.deps.URLs.ReturnURL(req.ProviderRef), "redirect": true,
		"prefill": map[string]any{"name": req.Customer.Name, "email": req.Customer.Email, "contact": req.Customer.Phone},
	}
	if req.Description != "" {
		opts["description"] = req.Description
	}
	if r.cfg.Logo != "" {
		opts["image"] = r.cfg.Logo
	}
	return connector.CheckoutResult{
		ProviderOrderID: sub.ID,
		NextAction:      connector.NextAction{Kind: connector.ActionSDK, ScriptURL: checkoutScript, Options: opts},
	}, nil
}

// FetchSubscription reads the subscription and the price of its plan.
func (r *razorpay) FetchSubscription(ctx context.Context, ref connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	if ref.ProviderSubscriptionID == "" {
		return connector.SubscriptionSnapshot{}, errors.New("razorpay: SubscriptionRef has no id")
	}
	var s subEntity
	if err := r.api.do(ctx, "GET", "/v1/subscriptions/"+url.PathEscape(ref.ProviderSubscriptionID), nil, nil, &s, nil); err != nil {
		return connector.SubscriptionSnapshot{}, err
	}
	return r.subSnapshot(ctx, s, true)
}

// CancelSubscription cancels now, or at the end of the current cycle.
func (r *razorpay) CancelSubscription(ctx context.Context, ref connector.SubscriptionRef, atPeriodEnd bool) error {
	flag := 0
	if atPeriodEnd {
		flag = 1
	}
	return r.api.do(ctx, "POST", "/v1/subscriptions/"+url.PathEscape(ref.ProviderSubscriptionID)+"/cancel", nil,
		map[string]any{"cancel_at_cycle_end": flag}, nil, nil)
}

// UpdateSubscriptionAmount moves the subscription to a plan with the new
// amount (same currency and cadence), effective from the next cycle with no
// proration.
func (r *razorpay) UpdateSubscriptionAmount(ctx context.Context, ref connector.SubscriptionRef, amount int64) error {
	if amount <= 0 {
		return errors.New("razorpay: amount must be positive")
	}
	var s subEntity
	if err := r.api.do(ctx, "GET", "/v1/subscriptions/"+url.PathEscape(ref.ProviderSubscriptionID), nil, nil, &s, nil); err != nil {
		return err
	}
	cur, err := r.planDetails(ctx, s.PlanID)
	if err != nil {
		return err
	}
	cur.Amount = amount
	planID, err := r.planFor(ctx, cur)
	if err != nil {
		return err
	}
	if planID == s.PlanID {
		return nil
	}
	body := map[string]any{"plan_id": planID, "schedule_change_at": "cycle_end"}
	err = r.api.do(ctx, "PATCH", "/v1/subscriptions/"+url.PathEscape(ref.ProviderSubscriptionID), nil, body, nil, nil)
	if err != nil && strings.Contains(err.Error(), "payment mode is UPI") {
		return fmt.Errorf("razorpay: a UPI subscription cannot be re-priced: %w", connector.ErrUnsupported)
	}
	return err
}

var _ connector.SubscriptionManager = (*razorpay)(nil)
