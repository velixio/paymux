package subscriptions

import (
	"context"
	"errors"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
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
// TODO(spec): whether PayU and Zoho reverse the verification debit themselves
// is assumed (both connectors declare true); confirm at sandbox time.

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
		if conn.Info().RefundsVerificationDebit || len(refunds) > 0 {
			continue
		}
		if _, err := s.core.CreateRefund(ctx, core.CreateRefundInput{Scope: core.Scope{AppID: sub.AppID, Mode: sub.Mode}, Actor: audit.System,
			InitiatedBy: "system", PaymentID: p.ID, Reason: ReasonMandateVerification}); err != nil {
			return err
		}
	}
	return nil
}
