package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/store"
)

// Payment flags set by refunds.
const (
	FlagRefundedPartial = "refunded_partial"
	FlagRefundedFull    = "refunded_full"
	// FlagRefundExceeds: the provider reports more refunded than was received.
	FlagRefundExceeds = "refund_exceeds_balance"
	// FlagDuplicateRefunded: the extra payment of a double payment was paid back.
	// It says nothing about the sale, which stays as paid.
	FlagDuplicateRefunded = "duplicate_refunded"
)

// RefundSnapshot is the authoritative provider state of a refund.
type RefundSnapshot struct {
	Status           state.RefundStatus
	ProviderRefundID string
	FailureReason    string
}

// RefundApplyResult reports what ApplyRefundSnapshot did.
type RefundApplyResult struct {
	Refund  store.Refund
	Payment store.Payment
	Changed bool
}

// ApplyRefundSnapshot moves a refund to the provider's state. On success it
// adds the amount to the payment's amount_refunded (setting refunded_partial
// or refunded_full), appends a negative refund ledger entry, bumps the payment
// version and emits refund.succeeded; on failure it emits refund.failed and
// frees the balance. Repeats are no-ops; flipping one terminal state to the
// other is ErrInvalidState. Everything commits in one transaction.
func (s *Service) ApplyRefundSnapshot(ctx context.Context, refundID string, snap RefundSnapshot) (RefundApplyResult, error) {
	if !slices.Contains(state.AllRefundStatuses, snap.Status) {
		return RefundApplyResult{}, newErr(ErrInvalid, "unknown refund status %q", snap.Status)
	}
	var res RefundApplyResult
	var alerts []notify.Alert
	err := s.db.Write(ctx, func(tx store.Tx) error {
		var err error
		alerts = nil
		res, err = s.applyRefundTx(ctx, tx, refundID, snap, &alerts)
		return err
	})
	if err == nil {
		s.raise(ctx, alerts)
	}
	return res, err
}

// applyRefundTx is ApplyRefundSnapshot inside the caller's transaction. Any
// ops alerts are appended to alerts, to be raised after commit.
func (s *Service) applyRefundTx(ctx context.Context, tx store.Tx, refundID string, snap RefundSnapshot, alerts *[]notify.Alert) (RefundApplyResult, error) {
	var res RefundApplyResult
	{
		r, err := store.GetRefund(ctx, tx, refundID)
		if err != nil {
			return res, mapStoreErr(err, "refund")
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, r.PaymentID)
		if err != nil {
			return res, mapStoreErr(err, "payment")
		}
		if r, err = store.GetRefund(ctx, tx, refundID); err != nil { // re-read under the lock
			return res, err
		}
		res.Refund, res.Payment = r, p
		switch state.RefundNext(state.RefundStatus(r.Status), snap.Status) {
		case state.Reject:
			return res, newErr(ErrInvalidState, "refund cannot move from %s to %s", r.Status, snap.Status)
		case state.Ignore:
			// A repeat may still carry the provider id we did not have yet (or
			// only had as our own reference standing in for it).
			if snap.ProviderRefundID != "" && snap.ProviderRefundID != r.ProviderRefundID && (r.ProviderRefundID == "" || r.ProviderRefundID == r.ProviderRef) {
				r.ProviderRefundID = snap.ProviderRefundID
				res.Refund = r
				return res, store.UpdateRefund(ctx, tx, r, s.nowUTC())
			}
			return res, nil
		}
		now := s.nowUTC()
		r.Status = string(snap.Status)
		r.ProviderRefundID = firstNonEmpty(snap.ProviderRefundID, r.ProviderRefundID)
		if err := store.UpdateRefund(ctx, tx, r, now); err != nil {
			return res, err
		}
		typ := "refund.failed"
		exceeds := false
		if snap.Status == state.RefundSucceeded {
			// The refund of the extra attempt of a double payment gives the customer
			// their second payment back. The sale itself was paid once and stays
			// as it is: no amount_refunded, no refunded flags, no credit note, and
			// no refund.succeeded for the app to mistake for a refund of the sale.
			att, aerr := store.GetAttempt(ctx, tx, r.AttemptID)
			if aerr != nil {
				return res, mapStoreErr(aerr, "attempt")
			}
			if att.Duplicate {
				addFlag(&p, FlagDuplicateRefunded)
				if err := store.UpdatePayment(ctx, tx, &p, now); err != nil {
					return res, err
				}
				// Same ledger kind, linked to the refund row, so the extra charge nets out.
				if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: ids.New("led"), OrgID: r.OrgID, Mode: r.Mode,
					Kind: "refund", Amount: -r.Amount, Currency: r.Currency, PaymentID: p.ID, RefundID: r.ID, AttemptID: r.AttemptID, OccurredAt: now}); err != nil {
					return res, err
				}
				res.Refund, res.Payment, res.Changed = r, p, true
				return res, s.emitPayment(ctx, tx, "payment.duplicate_refunded", p, "duplicate_payment", r.AttemptID)
			}
			typ = "refund.succeeded"
			p.AmountRefunded += r.Amount
			p.Flags = slices.DeleteFunc(p.Flags, func(f string) bool { return f == FlagRefundedPartial || f == FlagRefundedFull })
			if p.AmountRefunded >= p.AmountReceived {
				addFlag(&p, FlagRefundedFull)
			} else {
				addFlag(&p, FlagRefundedPartial)
			}
			// Only a refund the provider made on its own can overrun the balance
			// (ours are checked when created). The truth is still recorded.
			if exceeds = p.AmountRefunded > p.AmountReceived; exceeds {
				addFlag(&p, FlagRefundExceeds)
			}
			if err := store.UpdatePayment(ctx, tx, &p, now); err != nil {
				return res, err
			}
			if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: ids.New("led"), OrgID: r.OrgID, Mode: r.Mode,
				Kind: "refund", Amount: -r.Amount, Currency: r.Currency, PaymentID: p.ID, RefundID: r.ID, AttemptID: r.AttemptID, OccurredAt: now}); err != nil {
				return res, err
			}
			// Issues the GST credit note (when the payment has an invoice) in
			// this same transaction.
			if err := s.RefundSucceeded(ctx, tx, r); err != nil {
				return res, err
			}
		}
		res.Refund, res.Payment, res.Changed = r, p, true
		if err := s.emitter.Emit(ctx, tx, events.Event{OrgID: r.OrgID, AppID: r.AppID, Mode: r.Mode, Type: typ,
			ObjectType: "refund", ObjectID: r.ID, ObjectVersion: 2, Data: refundEventData{ViewRefund(r), snap.FailureReason}}); err != nil {
			return res, err
		}
		if exceeds {
			*alerts = append(*alerts, notify.Alert{Severity: notify.SeverityCritical, Kind: "refund.exceeds_balance",
				Message: fmt.Sprintf("The provider has refunded %d %s of payment %s, but only %d %s was received. The refund is recorded; check the payment in the dashboard.",
					p.AmountRefunded, r.Currency, p.ID, p.AmountReceived, r.Currency),
				Refs: map[string]string{"payment": p.ID, "refund": r.ID, "app": p.AppID}})
			if err := s.emitPayment(ctx, tx, "payment.needs_review", p, FlagRefundExceeds, r.AttemptID); err != nil {
				return res, err
			}
		}
		return res, nil
	}
}

// FailUnsentRefunds fails, inside the caller's transaction, the payment's
// pending refunds of ours whose refund.submit job has never been claimed by a
// worker, each through the ordinary failure path (refund.failed, the balance
// freed). The job is cancelled in the same transaction, atomically with the
// claim, so it never runs. A refund whose job has been tried, is running or is
// done may be with the provider already; failing it would be final while the
// provider's success could not move it, so it is left alone (the submit-time
// check stops any further send, and the provider's answer settles it). Returns
// how many it failed.
func (s *Service) FailUnsentRefunds(ctx context.Context, tx store.Tx, paymentID, reason string) (int, error) {
	rs, err := store.ListRefunds(ctx, tx, paymentID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rs {
		if r.Status != string(state.RefundPending) || r.ProviderRefundID != "" || r.InitiatedBy == "provider" {
			continue
		}
		if cancelled, err := store.CancelUnclaimedJob(ctx, tx, JobRefundSubmit+":"+r.ID, s.nowUTC()); err != nil {
			return n, err
		} else if !cancelled {
			continue
		}
		var alerts []notify.Alert // a failure raises none
		if _, err := s.applyRefundTx(ctx, tx, r.ID, RefundSnapshot{Status: state.RefundFailed, FailureReason: reason}, &alerts); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// refundEventData adds the failure reason to a refund snapshot.
type refundEventData struct {
	RefundView
	FailureReason string `json:"failure_reason,omitempty"`
}

// ProviderRefund is a refund the provider made on its own (from its own
// dashboard, or a Resolution Center), as the provider's API reports it. Its
// amount and currency come from that fetch, never from a webhook body.
type ProviderRefund struct {
	ProviderRefundID string
	Status           state.RefundStatus
	Amount           int64
	Currency         string
	FailureReason    string
}

// ImportedRefund is the outcome of ImportProviderRefund.
type ImportedRefund struct {
	RefundApplyResult
	// Created is true when a new refund row was made.
	Created bool
}

// ImportProviderRefund books a refund the provider made on its own, so that
// amount_refunded, the ledger, the credit note and the app's events follow what
// the provider did. It runs in one transaction under the payment lock and is
// idempotent on (attempt, provider refund id): the same refund again, by
// webhook or by reconciliation, finds its row and only advances it. A pending
// refund of ours with the same amount and no provider id yet is adopted
// instead of being counted twice. No refund.submit job is queued: nothing is
// sent to the provider. A refund beyond the balance is still recorded, and
// flagged (see FlagRefundExceeds).
func (s *Service) ImportProviderRefund(ctx context.Context, attemptID string, in ProviderRefund) (ImportedRefund, error) {
	if in.ProviderRefundID == "" {
		return ImportedRefund{}, newErr(ErrInvalid, "a provider refund needs the provider's refund id")
	}
	if !slices.Contains(state.AllRefundStatuses, in.Status) {
		return ImportedRefund{}, newErr(ErrInvalid, "unknown refund status %q", in.Status)
	}
	if in.Amount <= 0 || in.Amount > maxAmount {
		return ImportedRefund{}, newErr(ErrInvalid, "a provider refund needs a positive amount")
	}
	var out ImportedRefund
	var alerts []notify.Alert
	err := s.db.Write(ctx, func(tx store.Tx) error {
		out, alerts = ImportedRefund{}, nil
		a, err := store.GetAttempt(ctx, tx, attemptID)
		if err != nil {
			return mapStoreErr(err, "attempt")
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, a.PaymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		if a.Status != string(state.AttemptCaptured) {
			return newErr(ErrNotRefundable, "attempt %s was never captured, so it has nothing to refund", a.ID)
		}
		if !strings.EqualFold(in.Currency, a.CurrencyReceived) {
			return newErr(ErrInvalid, "the provider refunded in %s but the attempt was paid in %s", in.Currency, a.CurrencyReceived)
		}
		snap := RefundSnapshot{Status: in.Status, ProviderRefundID: in.ProviderRefundID, FailureReason: in.FailureReason}

		r, err := store.FindRefundByProviderID(ctx, tx, a.ID, in.ProviderRefundID)
		if errors.Is(err, store.ErrNotFound) {
			r, err = store.FindAdoptableRefund(ctx, tx, a.ID, in.Amount)
		}
		switch {
		case err == nil:
			out.RefundApplyResult, err = s.applyRefundTx(ctx, tx, r.ID, snap, &alerts)
			return err
		case !errors.Is(err, store.ErrNotFound):
			return err
		}

		now := s.nowUTC()
		r = store.Refund{ID: ids.New("ref"), OrgID: p.OrgID, AppID: p.AppID, Mode: p.Mode, PaymentID: p.ID, AttemptID: a.ID,
			Amount: in.Amount, Currency: a.CurrencyReceived, Status: string(state.RefundPending), ProviderRef: ids.ProviderRef(),
			ProviderRefundID: in.ProviderRefundID, InitiatedBy: "provider", CreatedAt: now, UpdatedAt: now}
		if err := store.InsertRefund(ctx, tx, r); err != nil {
			return err
		}
		if err := s.emitter.Emit(ctx, tx, events.Event{OrgID: r.OrgID, AppID: r.AppID, Mode: r.Mode, Type: "refund.created",
			ObjectType: "refund", ObjectID: r.ID, ObjectVersion: 1, Data: ViewRefund(r)}); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Entry{OrgID: r.OrgID, Actor: audit.System, Action: "refund.import", Target: r.ID,
			After: ViewRefund(r)}); err != nil {
			return err
		}
		out.Created = true
		out.RefundApplyResult, err = s.applyRefundTx(ctx, tx, r.ID, snap, &alerts)
		out.Created = true
		return err
	})
	if err == nil {
		s.raise(ctx, alerts)
	}
	return out, err
}
