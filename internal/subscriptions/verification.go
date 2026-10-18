package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// Verification debit. A registration with nothing due yet (a trial, or a
// re-authorization or new payment method on a running subscription) debits the
// provider's minimal verification amount (Info.MandateSetupAmount). It is not
// a supply: it is recorded as a `mandate_setup` Payment, which gets no invoice,
// and, unless the provider reverses it on its own
// (Info.RefundsVerificationDebit), it is refunded through the ordinary refund
// path (initiated_by=system, reason=mandate_verification).
//
// Zoho does not reverse it (its help pages: the Rs 1 is settled in the next
// payout), so Paymux refunds it. Whether PayU reverses it on its own is not
// known (its connector declares that it does). Unless the org setting
// verification_debit_fallback is "never", Paymux therefore checks 72 hours
// after the debit: if the provider shows the payment as not refunded and nobody
// has refunded it, Paymux refunds it explicitly. That refund is idempotent (the
// job is keyed by the payment; a refund row of any origin, or a provider that
// reports the money back, stops it; the balance check stops a second one).

// ReasonMandateVerification is the refund reason of a verification debit.
const ReasonMandateVerification = "mandate_verification"

// bookVerification records, inside tx, the verification payment of a
// registration: payment and attempt exist (attempt still "created") and a
// cycle row keyed by the provider payment id makes it happen once.
// settleVerification captures it after tx commits.
func (s *Service) bookVerification(ctx context.Context, tx store.Tx, sub store.Subscription, setup *connector.PaymentSnapshot) error {
	if setup == nil || setup.Status != connector.PaymentSucceeded || setup.ProviderPaymentID == "" || setup.Amount <= 0 {
		return nil
	}
	if _, err := store.GetCycleByProviderPayment(ctx, tx, sub.ID, setup.ProviderPaymentID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	p, a, err := s.core.CreateRenewalPayment(ctx, tx, core.RenewalInput{Kind: "mandate_setup", OrgID: sub.OrgID, AppID: sub.AppID,
		Mode: sub.Mode, SubscriptionID: sub.ID, CustomerID: sub.CustomerID, Description: "Mandate verification", ReturnURL: sub.ReturnURL,
		Metadata: sub.Metadata, Amount: setup.Amount, Currency: firstNonEmpty(setup.Currency, sub.Currency), ProviderAccountID: sub.ProviderAccountID})
	if err != nil {
		return err
	}
	now := s.nowUTC()
	return store.InsertCycle(ctx, tx, store.Cycle{ID: ids.New("cyc"), SubscriptionID: sub.ID, PeriodIndex: -1,
		ProviderPaymentID: setup.ProviderPaymentID, PaymentID: p.ID, AttemptID: a.ID, Amount: setup.Amount, State: "awaiting",
		CreatedAt: now, UpdatedAt: now})
}

// settleVerification captures the verification payment of every subscription
// on the mandate and refunds it when the provider does not reverse it. It is
// idempotent and safe to repeat after a crash.
func (s *Service) settleVerification(ctx context.Context, mandateID string, setup *connector.PaymentSnapshot) error {
	if setup == nil || setup.Status != connector.PaymentSucceeded || setup.ProviderPaymentID == "" {
		return nil
	}
	var subs []store.Subscription
	if err := s.db.Read(ctx, func(q store.Querier) (err error) { subs, err = store.SubscriptionsByMandate(ctx, q, mandateID); return }); err != nil {
		return err
	}
	for _, sub := range subs {
		var (
			cy      store.Cycle
			p       store.Payment
			att     store.Attempt
			refunds []store.Refund
		)
		err := s.db.Read(ctx, func(q store.Querier) error {
			var err error
			if cy, err = store.GetCycleByProviderPayment(ctx, q, sub.ID, setup.ProviderPaymentID); err != nil {
				return err
			}
			if p, err = store.GetPayment(ctx, q, cy.PaymentID); err != nil {
				return err
			}
			if att, err = store.GetAttempt(ctx, q, cy.AttemptID); err != nil {
				return err
			}
			refunds, err = store.ListRefunds(ctx, q, p.ID)
			return err
		})
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if p.Kind != "mandate_setup" {
			continue
		}
		if att.Status == string(state.AttemptCreated) {
			if _, err := s.core.ApplyAttemptSnapshot(ctx, att.ID, toSnapshot(*setup, cy.Amount, p.Currency)); err != nil {
				return err
			}
		}
		if cy.State == "awaiting" {
			if err := s.db.Write(ctx, func(tx store.Tx) error {
				if _, err := s.lockSub(ctx, tx, sub.ID); err != nil {
					return err
				}
				cy.State = "succeeded"
				return store.UpdateCycle(ctx, tx, cy, s.nowUTC())
			}); err != nil {
				return err
			}
		}
		conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
		if err != nil {
			return err
		}
		if len(refunds) > 0 {
			continue
		}
		if conn.Info().RefundsVerificationDebit {
			if err := s.scheduleVerificationFallback(ctx, sub, p, cy); err != nil {
				return err
			}
			continue
		}
		if _, err := s.core.CreateRefund(ctx, core.CreateRefundInput{Scope: core.Scope{AppID: sub.AppID, Mode: sub.Mode}, Actor: audit.System,
			InitiatedBy: "system", PaymentID: p.ID, Reason: ReasonMandateVerification}); err != nil {
			return err
		}
	}
	return nil
}

// verificationFallbackDelay is how long the provider has to reverse the debit.
const verificationFallbackDelay = 72 * time.Hour

// scheduleVerificationFallback queues the 72-hour check (once per payment),
// unless the org setting turns it off.
func (s *Service) scheduleVerificationFallback(ctx context.Context, sub store.Subscription, p store.Payment, cy store.Cycle) error {
	return s.db.Write(ctx, func(tx store.Tx) error {
		mode, err := resolve[string](ctx, tx, sub.AppID, "verification_debit_fallback")
		if err != nil {
			return err
		}
		if mode == "never" {
			return nil
		}
		return s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobVerificationFallback, RunAt: cy.CreatedAt.Add(verificationFallbackDelay),
			Payload: map[string]string{"payment_id": p.ID, "subscription_id": sub.ID}, DedupeKey: JobVerificationFallback + ":" + p.ID})
	})
}

// handleVerificationFallback refunds a verification debit the provider has not
// reversed. It does nothing when a refund of the payment exists in any state
// but failed (an operator's, the provider's imported one, or its own earlier
// run), when the provider shows the money as returned, or when the balance is
// already refunded.
func (s *Service) handleVerificationFallback(ctx context.Context, raw json.RawMessage) error {
	var pl struct {
		PaymentID      string `json:"payment_id"`
		SubscriptionID string `json:"subscription_id"`
	}
	if err := json.Unmarshal(raw, &pl); err != nil || pl.PaymentID == "" {
		return jobs.Permanent(errors.New("subscription.verification_fallback: bad payload"))
	}
	var (
		p       store.Payment
		sub     store.Subscription
		att     store.Attempt
		refunds []store.Refund
	)
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		if p, err = store.GetPayment(ctx, q, pl.PaymentID); err != nil {
			return err
		}
		if sub, err = store.GetSubscription(ctx, q, pl.SubscriptionID); err != nil {
			return err
		}
		attempts, err := store.ListAttempts(ctx, q, p.ID)
		if err != nil {
			return err
		}
		for _, a := range attempts {
			if a.Status == string(state.AttemptCaptured) {
				att = a
			}
		}
		refunds, err = store.ListRefunds(ctx, q, p.ID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.Kind != "mandate_setup" || p.Status != string(state.PaymentSucceeded) || att.ID == "" {
		return nil
	}
	for _, r := range refunds {
		if r.Status != string(state.RefundFailed) {
			return nil // refunded, or being refunded, by someone
		}
	}
	conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
	if err != nil {
		return err
	}
	snap, err := conn.FetchPayment(ctx, connector.PaymentRef{ProviderRef: att.ProviderRef, ProviderOrderID: att.ProviderOrderID, ProviderPaymentID: att.ProviderPaymentID})
	switch {
	case err == nil:
		if snap.AmountRefunded >= p.AmountReceived && p.AmountReceived > 0 {
			return nil // the provider reversed it itself
		}
	case errors.Is(err, connector.ErrNotFound):
		// The provider cannot show it; the refund below is the safe way to find out
		// (a provider that already returned it will refuse, and the refund will fail visibly).
	default:
		return err // could not ask: the queue retries
	}
	_, err = s.core.CreateRefund(ctx, core.CreateRefundInput{Scope: core.Scope{AppID: sub.AppID, Mode: sub.Mode}, Actor: audit.System,
		InitiatedBy: "system", PaymentID: p.ID, Reason: ReasonMandateVerification})
	if errors.Is(err, core.ErrRefundExceeds) || errors.Is(err, core.ErrNotRefundable) {
		return nil // already refunded, or not refundable (a dispute): nothing more to do
	}
	return err
}
