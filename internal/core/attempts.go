package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Payment flags set by the state machine.
const (
	FlagDuplicateSuccess = "duplicate_success"
	FlagLateSuccess      = "late_success"
)

// CreateAttempt records a new try of a payment at a provider account (status
// created). Attempts are refused on canceled and expired payments; a new
// attempt on a succeeded payment is allowed because a customer can pay twice,
// which ApplyAttemptSnapshot then handles as a double success.
func (s *Service) CreateAttempt(ctx context.Context, paymentID, providerAccountID string) (store.Attempt, error) {
	var a store.Attempt
	err := s.db.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPaymentForUpdate(ctx, tx, paymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		switch state.PaymentStatus(p.Status) {
		case state.PaymentCanceled, state.PaymentExpired:
			return newErr(ErrInvalidState, "a payment that is %s cannot take new attempts", p.Status)
		}
		pa, err := store.GetProviderAccount(ctx, tx, providerAccountID)
		if err != nil {
			return mapStoreErr(err, "provider account")
		}
		if pa.Mode != p.Mode || pa.OrgID != p.OrgID {
			return newErr(ErrInvalid, "provider account is in another mode")
		}
		now := s.nowUTC()
		a = store.Attempt{ID: ids.New("att"), OrgID: p.OrgID, Mode: p.Mode, PaymentID: p.ID, ProviderAccountID: pa.ID,
			ProviderRef: ids.ProviderRef(), Status: string(state.AttemptCreated), CreatedAt: now, UpdatedAt: now}
		return store.InsertAttempt(ctx, tx, a)
	})
	return a, err
}

// Snapshot is the authoritative provider state of one attempt, as fetched by a
// connector. Amount and Currency are what the provider says it received and
// are required when Status is captured.
type Snapshot struct {
	Status                      state.AttemptStatus
	Amount                      int64
	Currency                    string
	Method, ProviderPaymentID   string
	ProviderOrderID             string
	FailureCode, FailureMessage string
}

// ApplyResult reports what ApplyAttemptSnapshot did. Changed is false when
// the snapshot was stale or a repeat and nothing was written.
type ApplyResult struct {
	Payment store.Payment
	Attempt store.Attempt
	Changed bool
}

// paymentEventFor maps an attempt status to the payment-level event it implies.
// ok is false when the status implies none.
func paymentEventFor(snap Snapshot, p store.Payment, otherOpen bool) (state.PaymentEvent, bool) {
	switch snap.Status {
	case state.AttemptPending:
		return state.EvRequiresAction, true
	case state.AttemptAuthorized:
		return state.EvProcessing, true
	case state.AttemptCaptured:
		if snap.Amount != p.Amount || snap.Currency != p.Currency {
			return state.EvSucceededMismatch, true
		}
		return state.EvSucceeded, true
	case state.AttemptFailed:
		// One failed try does not fail the payment while another is open.
		return state.EvFailed, !otherOpen
	}
	return "", false
}

func addFlag(p *store.Payment, f string) {
	if !p.HasFlag(f) {
		p.Flags = append(p.Flags, f)
	}
}

func validateSnapshot(snap *Snapshot) error {
	if !slices.Contains(state.AllAttemptStatuses, snap.Status) {
		return newErr(ErrInvalid, "unknown attempt status %q", snap.Status)
	}
	if snap.Status == state.AttemptCaptured {
		snap.Currency = strings.ToUpper(snap.Currency)
		if _, err := money.Exponent(snap.Currency); err != nil {
			return newErr(ErrInvalid, "a captured snapshot needs a valid currency")
		}
		if snap.Amount <= 0 || snap.Amount > maxAmount {
			return newErr(ErrInvalid, "a captured snapshot needs a positive amount")
		}
	}
	return nil
}

// ApplyAttemptSnapshot moves an attempt, and through it its payment, to the
// provider state in snap. It is idempotent: stale and repeated snapshots
// change nothing. All effects (attempt and payment rows, version bump, ledger
// entries, events, refund and job for auto-refunds) commit in one transaction.
func (s *Service) ApplyAttemptSnapshot(ctx context.Context, attemptID string, snap Snapshot) (ApplyResult, error) {
	if err := validateSnapshot(&snap); err != nil {
		return ApplyResult{}, err
	}
	var res ApplyResult
	err := s.db.Write(ctx, func(tx store.Tx) error {
		res = ApplyResult{}
		a, err := store.GetAttempt(ctx, tx, attemptID)
		if err != nil {
			return mapStoreErr(err, "attempt")
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, a.PaymentID)
		if err != nil {
			return mapStoreErr(err, "payment")
		}
		if a, err = store.GetAttempt(ctx, tx, attemptID); err != nil { // re-read under the payment lock
			return err
		}
		res.Payment, res.Attempt = p, a

		ao := state.AttemptNext(state.AttemptStatus(a.Status), snap.Status)
		switch ao.Action {
		case state.Reject:
			return newErr(ErrInvalidState, "attempt cannot move from %s to %s", a.Status, snap.Status)
		case state.Ignore:
			return nil
		}
		now := s.nowUTC()
		res.Changed = true

		attempts, err := store.ListAttempts(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		otherOpen := false
		for _, o := range attempts {
			switch state.AttemptStatus(o.Status) {
			case state.AttemptCreated, state.AttemptPending, state.AttemptAuthorized:
				otherOpen = otherOpen || o.ID != a.ID
			}
		}

		a.Status = string(snap.Status)
		a.Method = firstNonEmpty(snap.Method, a.Method)
		a.ProviderPaymentID = firstNonEmpty(snap.ProviderPaymentID, a.ProviderPaymentID)
		a.ProviderOrderID = firstNonEmpty(snap.ProviderOrderID, a.ProviderOrderID)
		if snap.Status == state.AttemptFailed {
			a.FailureCode, a.FailureMessage = snap.FailureCode, snap.FailureMessage
		}
		if snap.Status == state.AttemptCaptured {
			a.AmountReceived, a.CurrencyReceived = snap.Amount, snap.Currency
			a.FailureCode, a.FailureMessage = "", ""
			if err := store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: ids.New("led"), OrgID: p.OrgID, Mode: p.Mode,
				Kind: "charge", Amount: snap.Amount, Currency: snap.Currency, PaymentID: p.ID, OccurredAt: now}); err != nil {
				return err
			}
		}

		ev, haveEvent := paymentEventFor(snap, p, otherOpen)
		if haveEvent {
			po := state.PaymentNext(state.PaymentStatus(p.Status), ev)
			switch po.Action {
			case state.Apply:
				if err := s.applyPaymentTransition(ctx, tx, &p, a.ID, ev, po, snap, now); err != nil {
					return err
				}
			case state.Duplicate:
				a.Duplicate = true
				if err := s.handleDuplicate(ctx, tx, &p, a, snap, now); err != nil {
					return err
				}
			}
		}
		if err := store.UpdateAttempt(ctx, tx, a, now); err != nil {
			return err
		}
		res.Payment, res.Attempt = p, a
		return nil
	})
	return res, err
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// applyPaymentTransition writes an Apply outcome: status, amount received,
// flags, version bump and the matching payment.* event.
func (s *Service) applyPaymentTransition(ctx context.Context, tx store.Tx, p *store.Payment, attemptID string,
	ev state.PaymentEvent, po state.PaymentOutcome, snap Snapshot, now time.Time) error {
	p.Status = string(po.Next)
	if ev == state.EvSucceeded || ev == state.EvSucceededMismatch {
		p.AmountReceived = snap.Amount
	}
	if po.Late {
		addFlag(p, FlagLateSuccess)
	}
	if po.Reason != "" {
		addFlag(p, po.Reason)
	}
	if err := store.UpdatePayment(ctx, tx, p, now); err != nil {
		return err
	}
	return s.emitPayment(ctx, tx, "payment."+p.Status, *p, po.Reason, attemptID)
}

// handleDuplicate deals with a second successful attempt on a succeeded
// payment, following the duplicate_payment_action setting.
func (s *Service) handleDuplicate(ctx context.Context, tx store.Tx, p *store.Payment, extra store.Attempt, snap Snapshot, now time.Time) error {
	addFlag(p, FlagDuplicateSuccess)
	if err := store.UpdatePayment(ctx, tx, p, now); err != nil {
		return err
	}
	v, err := settings.ResolveWith(ctx, tx, p.AppID, "duplicate_payment_action")
	if err != nil {
		return err
	}
	var action string
	if err := json.Unmarshal(v.Raw, &action); err != nil {
		return err
	}
	if action == "auto_refund" {
		// A full refund of the EXTRA attempt only. The duplicate is not a
		// supply, so it gets no invoice and its refund no credit note.
		// TODO(spec): raise an info alert (alerts are not built yet).
		_, err := s.insertRefund(ctx, tx, *p, extra, snap.Amount, "duplicate_payment", "system", audit.System)
		return err
	}
	// TODO(spec): raise an ops alert (alerts are not built yet); an operator
	// refunds the extra attempt from the dashboard.
	return s.emitPayment(ctx, tx, "payment.needs_review", *p, FlagDuplicateSuccess, extra.ID)
}
