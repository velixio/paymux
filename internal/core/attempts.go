package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Payment flags set by the state machine.
const (
	FlagDuplicateSuccess = "duplicate_success"
	FlagLateSuccess      = "late_success"
)

// MaxAttempts bounds the attempts of one payment. Each attempt is a checkout
// created at a provider, and the pay page makes one whenever the customer asks
// to try again: without a bound, anyone holding the pay link could create rows
// and provider calls at will.
const MaxAttempts = 30

// CreateAttempt records a new try of a payment at a provider account (status
// created). Attempts are refused on canceled and expired payments, and once the
// payment has MaxAttempts; a new attempt on a succeeded payment is allowed
// because a customer can pay twice, which ApplyAttemptSnapshot then handles as
// a double success.
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
		// Counted under the payment lock, so concurrent requests cannot pass together.
		if prior, err := store.ListAttempts(ctx, tx, p.ID); err != nil {
			return err
		} else if len(prior) >= MaxAttempts {
			return newErr(ErrInvalidState, "the payment has had %d attempts; no more can be started", len(prior))
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
	var alerts []notify.Alert
	err := s.db.Write(ctx, func(tx store.Tx) error {
		res, alerts = ApplyResult{}, nil
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
				Kind: "charge", Amount: snap.Amount, Currency: snap.Currency, PaymentID: p.ID, AttemptID: a.ID, OccurredAt: now}); err != nil {
				return err
			}
		}

		prevStatus := p.Status
		ev, haveEvent := paymentEventFor(snap, p, otherOpen)
		if haveEvent {
			po := state.PaymentNext(state.PaymentStatus(p.Status), ev)
			switch po.Action {
			case state.Apply:
				if err := s.applyPaymentTransition(ctx, tx, &p, a.ID, ev, po, snap, now, &alerts); err != nil {
					return err
				}
			case state.Duplicate:
				a.Duplicate = true
				if err := s.handleDuplicate(ctx, tx, &p, a, snap, now, &alerts); err != nil {
					return err
				}
			}
		}
		if err := store.UpdateAttempt(ctx, tx, a, now); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return errProviderPaymentBound
			}
			return err
		}
		if s.onPaymentSucceeded != nil && prevStatus != string(state.PaymentSucceeded) &&
			p.Status == string(state.PaymentSucceeded) && !a.Duplicate {
			if err := s.onPaymentSucceeded(context.WithValue(ctx, alertSinkKey{}, &alerts), tx, p, a); err != nil {
				return err
			}
		}
		res.Payment, res.Attempt = p, a
		return nil
	})
	if errors.Is(err, errProviderPaymentBound) {
		// Another attempt of the account settled this provider payment first, in
		// a transaction that committed after our read check. Nothing of ours was
		// written: the snapshot is not this attempt's, as when that check rejects it.
		slog.Warn("core: the provider payment already settles another attempt; snapshot ignored", "attempt", attemptID,
			"provider_payment", snap.ProviderPaymentID)
		return s.unchanged(ctx, attemptID)
	}
	if err == nil {
		s.raise(ctx, alerts)
	}
	return res, err
}

// errProviderPaymentBound means the unique index on a captured attempt's
// provider payment refused the write.
var errProviderPaymentBound = errors.New("core: provider payment already bound to another attempt")

// unchanged reports an attempt and its payment as they are stored.
func (s *Service) unchanged(ctx context.Context, attemptID string) (ApplyResult, error) {
	var res ApplyResult
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		if res.Attempt, err = store.GetAttempt(ctx, q, attemptID); err != nil {
			return err
		}
		res.Payment, err = store.GetPayment(ctx, q, res.Attempt.PaymentID)
		return
	})
	return res, mapStoreErr(err, "attempt")
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
	ev state.PaymentEvent, po state.PaymentOutcome, snap Snapshot, now time.Time, alerts *[]notify.Alert) error {
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
	if po.Next == state.PaymentNeedsReview {
		sev, msg := notify.SeverityWarning, fmt.Sprintf("Payment %s needs review: the provider reports %d %s but %d %s was expected. Check it in the dashboard.",
			p.ID, snap.Amount, snap.Currency, p.Amount, p.Currency)
		if po.Reason == state.ReasonSuccessAfterCancel {
			sev, msg = notify.SeverityCritical, fmt.Sprintf("Payment %s was paid (%d %s) after the app canceled it. It needs review and is never refunded automatically.",
				p.ID, snap.Amount, snap.Currency)
		}
		*alerts = append(*alerts, notify.Alert{Severity: sev, Kind: "payment.needs_review", Message: msg,
			Refs: map[string]string{"payment": p.ID, "attempt": attemptID, "app": p.AppID, "reason": po.Reason}})
	}
	return s.emitPayment(ctx, tx, "payment."+p.Status, *p, po.Reason, attemptID)
}

// handleDuplicate deals with a second successful attempt on a succeeded
// payment, following the duplicate_payment_action setting.
func (s *Service) handleDuplicate(ctx context.Context, tx store.Tx, p *store.Payment, extra store.Attempt, snap Snapshot, now time.Time, alerts *[]notify.Alert) error {
	addFlag(p, FlagDuplicateSuccess)
	if err := store.UpdatePayment(ctx, tx, p, now); err != nil {
		return err
	}
	v, err := s.policy(ctx, tx, p.AppID, "duplicate_payment_action")
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
		_, err := s.insertRefund(ctx, tx, *p, extra, snap.Amount, "duplicate_payment", "system", "", audit.System)
		if err == nil {
			*alerts = append(*alerts, notify.Alert{Severity: notify.SeverityInfo, Kind: "payment.duplicate_refunded",
				Message: fmt.Sprintf("Payment %s was paid twice; the extra %d %s is being refunded automatically.", p.ID, snap.Amount, snap.Currency),
				Refs:    map[string]string{"payment": p.ID, "attempt": extra.ID, "app": p.AppID}})
		}
		return err
	}
	// An operator refunds the extra attempt from the dashboard.
	*alerts = append(*alerts, notify.Alert{Severity: notify.SeverityWarning, Kind: "payment.duplicate_success",
		Message: fmt.Sprintf("Payment %s was paid twice (extra %d %s). Refund the extra attempt from the dashboard.", p.ID, snap.Amount, snap.Currency),
		Refs:    map[string]string{"payment": p.ID, "attempt": extra.ID, "app": p.AppID}})
	return s.emitPayment(ctx, tx, "payment.needs_review", *p, FlagDuplicateSuccess, extra.ID)
}
