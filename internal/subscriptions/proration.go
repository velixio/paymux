package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/paymux/paymux/internal/billing"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Proration of a mid-period amount change (merchant-triggered subscriptions).
//
//	none          the new amount applies from the next period; nothing else.
//	next_invoice  the difference for the rest of the current period, by
//	              remaining seconds, is added to (an upgrade) or taken off (a
//	              downgrade) the next renewal.
//	immediate     an upgrade's difference is charged as soon as the rules allow:
//	              a pre-debit notice now and the debit at least 24h later, as a
//	              separate prorated charge that does not renew anything. A
//	              downgrade credits the next renewal, as with next_invoice.
//
// A credit is never refunded; one larger than the next renewal carries over to
// the ones after it. The prorated lines are kept on the cycle and shown on its
// invoice. Provider-managed subscriptions are billed by the provider, which
// cannot be told to add a one-off difference, so they refuse proration.
//
// TODO(spec): a credit never takes a renewal below 1 minor unit; providers have
// their own minimum debit that this does not know.

// Proration is how a plan change is billed.
type Proration string

// Proration modes.
const (
	ProrationNone        Proration = "none"
	ProrationNextInvoice Proration = "next_invoice"
	ProrationImmediate   Proration = "immediate"
)

// prorationBase is the first period index of prorated charges: far above any
// real period, so they never collide with a billing period and never count as
// one.
const prorationBase = 1_000_000

func isProration(periodIndex int) bool { return periodIndex >= prorationBase }

// UpdateInput is a plan change.
type UpdateInput struct {
	Amount    int64
	Proration Proration // default none
}

// ProrationOutcome says what a prorated change did.
type ProrationOutcome struct {
	// Requested is the mode asked for; Mode is what happened (an immediate
	// charge that cannot be made now falls back to next_invoice).
	Requested Proration `json:"requested"`
	Mode      Proration `json:"mode"`
	// Amount is the signed difference for the rest of the period: positive is a
	// charge, negative a credit, in minor units.
	Amount      int64  `json:"amount"`
	Description string `json:"description,omitempty"`
	// ChargeAt is when an immediate charge will be made, at the earliest.
	ChargeAt *time.Time `json:"charge_at,omitempty"`
	// Reason explains a fallback or a no-op.
	Reason string `json:"reason,omitempty"`
}

func adjustTotal(l []store.AdjustLine) (n int64) {
	for _, x := range l {
		n += x.Amount
	}
	return n
}

// prorationText describes a change for the invoice line.
func prorationText(oldAmt, newAmt int64, currency string, start, end, at time.Time) string {
	f := func(v int64) string {
		s, err := money.FormatDecimal(v, currency)
		if err != nil {
			return fmt.Sprint(v)
		}
		return s
	}
	total := end.Sub(start)
	left := end.Sub(at)
	if left > total {
		left = total
	}
	days := func(d time.Duration) string { return fmt.Sprintf("%.1f", d.Hours()/24) }
	return fmt.Sprintf("Proration: plan change from %s to %s %s, %s of %s days remaining", f(oldAmt), f(newAmt), currency, days(left), days(total))
}

// prorate applies the change to sub (the caller saves it). It returns what was done.
func (s *Service) prorate(ctx context.Context, tx store.Tx, sub *store.Subscription, newAmount int64, mode Proration) (*ProrationOutcome, error) {
	out := &ProrationOutcome{Requested: mode, Mode: mode}
	now := s.nowUTC()
	if sub.Status != string(state.SubActive) || !now.Before(sub.CurrentPeriodEnd) {
		out.Mode, out.Reason = ProrationNone, "no_paid_period_to_prorate"
		return out, nil
	}
	old := sub.BilledAmount
	if old == 0 {
		old = sub.Amount
	}
	diff := billing.Prorate(old, newAmount, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, now)
	sub.BilledAmount = newAmount
	out.Amount = diff
	if diff == 0 {
		out.Mode, out.Reason = ProrationNone, "no_difference"
		return out, nil
	}
	line := store.AdjustLine{Description: prorationText(old, newAmount, sub.Currency, sub.CurrentPeriodStart, sub.CurrentPeriodEnd, now), Amount: diff}
	out.Description = line.Description

	if mode == ProrationImmediate && diff > 0 {
		at, reason, err := s.scheduleProrationCharge(ctx, tx, sub, line)
		if err != nil {
			return nil, err
		}
		if reason == "" {
			out.ChargeAt = &at
			return out, nil
		}
		out.Mode, out.Reason = ProrationNextInvoice, reason // charged with the next renewal instead
	}
	// next_invoice, a downgrade, or an immediate charge that cannot be made now.
	sub.ProrationAdjust += diff
	sub.ProrationLines = append(sub.ProrationLines, line)
	if diff < 0 && mode == ProrationImmediate {
		out.Mode = ProrationNextInvoice
		out.Reason = "credit_applies_to_the_next_renewal"
	}
	return out, nil
}

// scheduleProrationCharge queues a separate charge of line.Amount, announced
// now and debited at least 24h later. reason is non-empty, and nothing is
// queued, when it cannot be done now.
func (s *Service) scheduleProrationCharge(ctx context.Context, tx store.Tx, sub *store.Subscription, line store.AdjustLine) (at time.Time, reason string, err error) {
	if sub.BillingMode != string(connector.MerchantTriggered) || sub.MandateID == "" {
		return at, "no_mandate", nil
	}
	if sub.RequiresReauthorization {
		return at, "mandate_needs_reauthorization", nil
	}
	m, err := s.mandateOf(ctx, tx, *sub)
	if err != nil {
		return at, "", err
	}
	now := s.nowUTC()
	if m.Status != string(state.MandateActive) || (!m.ValidUntil.IsZero() && !now.Before(m.ValidUntil)) {
		return at, "mandate_not_active", nil
	}
	if line.Amount > m.MaxAmount {
		return at, "amount_exceeds_mandate_max", nil
	}
	pol, err := s.loadPolicy(ctx, tx, sub.AppID)
	if err != nil {
		return at, "", err
	}
	cycles, err := store.ListCycles(ctx, tx, sub.ID)
	if err != nil {
		return at, "", err
	}
	n := 0
	for _, c := range cycles {
		if isProration(c.PeriodIndex) {
			n++
		}
	}
	cy := store.Cycle{ID: ids.New("cyc"), SubscriptionID: sub.ID, PeriodIndex: prorationBase + n, Amount: line.Amount,
		Adjustments: []store.AdjustLine{line}, State: "scheduled", CreatedAt: now, UpdatedAt: now}
	if err := store.InsertCycle(ctx, tx, cy); err != nil {
		return at, "", err
	}
	if err := s.queueCycle(ctx, tx, *sub, &cy, pol, m, now); err != nil {
		return at, "", err
	}
	// queueCycle fixed the debit time: the notice now, the debit 24h or more later.
	cy, err = store.GetCycle(ctx, tx, sub.ID, cy.PeriodIndex)
	return cy.NextRunAt, "", err
}

// applyAdjustments folds the subscription's pending proration into the amount
// of a renewal that is about to be billed (called when its payment is made).
// A charge is limited by the mandate's maximum and a credit leaves at least one
// minor unit to charge; what does not fit stays for the renewals after it.
func (s *Service) applyAdjustments(ctx context.Context, tx store.Tx, sub *store.Subscription, cy *store.Cycle) error {
	if sub.ProrationAdjust == 0 {
		return nil
	}
	base, adj := cy.Amount, sub.ProrationAdjust
	used := adj
	if adj > 0 {
		if sub.MandateID != "" {
			m, err := s.mandateOf(ctx, tx, *sub)
			if err != nil {
				return err
			}
			if m.MaxAmount > base {
				used = min(adj, m.MaxAmount-base)
			} else {
				used = 0
			}
		}
	} else {
		used = max(adj, -(base - 1)) // keep at least 1 minor unit to charge
	}
	if used == 0 {
		return nil
	}
	if used == adj {
		cy.Adjustments = append(cy.Adjustments, sub.ProrationLines...)
		sub.ProrationAdjust, sub.ProrationLines = 0, nil
	} else {
		cy.Adjustments = append(cy.Adjustments, store.AdjustLine{Description: "Plan change adjustment (part, the rest follows)", Amount: used})
		sub.ProrationAdjust = adj - used
		sub.ProrationLines = []store.AdjustLine{{Description: "Plan change adjustment (carried over)", Amount: adj - used}}
	}
	cy.Amount = base + used
	return s.saveSub(ctx, tx, sub)
}

// failProration ends a prorated charge that could not be made: the amount is
// carried to the next renewal instead, and the subscription is otherwise left
// alone (no dunning, no past-due: the customer's plan is paid up).
func (s *Service) failProration(ctx context.Context, tx store.Tx, sub *store.Subscription, cy *store.Cycle, reason string) error {
	now := s.nowUTC()
	cy.State, cy.LeaseUntil = "failed", time.Time{}
	if err := store.UpdateCycle(ctx, tx, *cy, now); err != nil {
		return err
	}
	if terminal(sub.Status) {
		return nil
	}
	sub.ProrationAdjust += cy.Amount
	sub.ProrationLines = append(sub.ProrationLines, cy.Adjustments...)
	if err := s.saveSub(ctx, tx, sub); err != nil {
		return err
	}
	return s.emitSub(ctx, tx, EvUpdated, *sub, eventData{Reason: "proration_charge_failed:" + reason})
}
