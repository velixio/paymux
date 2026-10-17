package core

import (
	"context"
	"slices"

	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/store"
)

// Payment flags set by refunds.
const (
	FlagRefundedPartial = "refunded_partial"
	FlagRefundedFull    = "refunded_full"
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
	err := s.db.Write(ctx, func(tx store.Tx) error {
		res = RefundApplyResult{}
		r, err := store.GetRefund(ctx, tx, refundID)
		if err != nil {
			return mapStoreErr(err, "refund")
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, r.PaymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		if r, err = store.GetRefund(ctx, tx, refundID); err != nil { // re-read under the lock
			return err
		}
		res.Refund, res.Payment = r, p
		switch state.RefundNext(state.RefundStatus(r.Status), snap.Status) {
		case state.Reject:
			return newErr(ErrInvalidState, "refund cannot move from %s to %s", r.Status, snap.Status)
		case state.Ignore:
			// A repeat may still carry the provider id we did not have yet.
			if r.ProviderRefundID == "" && snap.ProviderRefundID != "" {
				r.ProviderRefundID = snap.ProviderRefundID
				res.Refund = r
				return store.UpdateRefund(ctx, tx, r, s.nowUTC())
			}
			return nil
		}
		now := s.nowUTC()
		r.Status = string(snap.Status)
		r.ProviderRefundID = firstNonEmpty(snap.ProviderRefundID, r.ProviderRefundID)
		if err := store.UpdateRefund(ctx, tx, r, now); err != nil {
			return err
		}
		typ := "refund.failed"
		if snap.Status == state.RefundSucceeded {
			typ = "refund.succeeded"
			p.AmountRefunded += r.Amount
			p.Flags = slices.DeleteFunc(p.Flags, func(f string) bool { return f == FlagRefundedPartial || f == FlagRefundedFull })
			if p.AmountRefunded >= p.AmountReceived {
				addFlag(&p, FlagRefundedFull)
			} else {
				addFlag(&p, FlagRefundedPartial)
			}
			if err := store.UpdatePayment(ctx, tx, &p, now); err != nil {
				return err
			}
			if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: ids.New("led"), OrgID: r.OrgID, Mode: r.Mode,
				Kind: "refund", Amount: -r.Amount, Currency: r.Currency, PaymentID: p.ID, RefundID: r.ID, OccurredAt: now}); err != nil {
				return err
			}
		}
		res.Refund, res.Payment, res.Changed = r, p, true
		return s.emitter.Emit(ctx, tx, events.Event{OrgID: r.OrgID, AppID: r.AppID, Mode: r.Mode, Type: typ,
			ObjectType: "refund", ObjectID: r.ID, ObjectVersion: 2, Data: refundEventData{ViewRefund(r), snap.FailureReason}})
	})
	return res, err
}

// refundEventData adds the failure reason to a refund snapshot.
type refundEventData struct {
	RefundView
	FailureReason string `json:"failure_reason,omitempty"`
}
