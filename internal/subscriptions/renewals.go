package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/paymux/paymux/internal/billing"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// Merchant-triggered renewal engine (spec 6.8).
//
// A cycle row exists per billed period k. Period 0 is charged when the
// subscription starts (at the anchor); period k>0 is charged at Boundary(k),
// after a pre-debit notice sent at Boundary(k) - predebit_lead. Cycle states:
//
//	scheduled -> charging -> succeeded
//	                      -> awaiting (provider has not settled) -> succeeded | failed
//	                      -> failed -> scheduled (dunning retry) | exhausted
//
// Every step runs under the subscription lock; the provider call itself is
// made outside it, after a lease on the cycle has been taken, and uses the
// attempt's provider_ref so a repeat is idempotent at the provider.

const (
	chargeLease  = 5 * time.Minute
	pollInterval = 30 * time.Minute
	maxPolls     = 200
)

// RegisterJobs registers the job handlers of this package on q.
func (s *Service) RegisterJobs(q *jobs.Queue) {
	q.Register(JobNotifyPreDebit, s.handleNotify)
	q.Register(JobChargeRenewal, s.handleCharge)
	q.Register(JobPollRenewal, s.handlePoll)
	q.Register(JobExpireIncomplete, s.handleExpireIncomplete)
	q.Register(JobMandateExpiry, s.handleMandateExpiry)
	q.Register(JobRevokeMandate, s.handleRevokeMandate)
	q.Register(JobCancelAtProvider, s.handleCancelAtProvider)
}

type cyclePayload struct {
	SubscriptionID string `json:"subscription_id"`
	Period         int    `json:"period"`
}

func (s *Service) mandateOf(ctx context.Context, q store.Queryer, sub store.Subscription) (store.Mandate, error) {
	return store.GetMandate(ctx, q, sub.MandateID)
}

// snap moves t out of the UPI peak windows for UPI mandates.
func (p policy) snap(method string, t time.Time) time.Time {
	if method == string(billing.MethodUPI) {
		return billing.SnapOutsidePeak(t, p.peaks)
	}
	return t
}

func (s *Service) enqueueCharge(ctx context.Context, tx store.Tx, sub store.Subscription, cy *store.Cycle, runAt time.Time) error {
	cy.JobSeq++
	step := "charge"
	if cy.JobSeq > 1 {
		step = fmt.Sprintf("charge-%d", cy.JobSeq)
	}
	cy.NextRunAt = runAt
	return s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobChargeRenewal, RunAt: runAt,
		Payload:   cyclePayload{SubscriptionID: sub.ID, Period: cy.PeriodIndex},
		DedupeKey: fmt.Sprintf("sub:%s:%d:%s", sub.ID, cy.PeriodIndex, step)})
}

func (s *Service) enqueueNotify(ctx context.Context, tx store.Tx, sub store.Subscription, k, round int, runAt time.Time) error {
	step := "notify"
	if round > 0 {
		step = fmt.Sprintf("notify-%d", round)
	}
	return s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobNotifyPreDebit, RunAt: runAt,
		Payload: cyclePayload{SubscriptionID: sub.ID, Period: k}, DedupeKey: fmt.Sprintf("sub:%s:%d:%s", sub.ID, k, step)})
}

// scheduleCycle creates the cycle of period k (once) and its jobs: the
// pre-debit notice and the charge. Period 0 needs no notice (the customer has
// just authorised the mandate).
func (s *Service) scheduleCycle(ctx context.Context, tx store.Tx, sub store.Subscription, k int) error {
	now := s.nowUTC()
	pol, err := s.loadPolicy(ctx, tx, sub.AppID)
	if err != nil {
		return err
	}
	m, err := s.mandateOf(ctx, tx, sub)
	if err != nil {
		return err
	}
	iv := billing.Interval(sub.Interval)
	chargeAt := billing.Boundary(sub.AnchorAt, iv, sub.IntervalCount, k)
	if chargeAt.Before(now) {
		chargeAt = now
	}
	amount := sub.Amount
	if k > 0 && sub.PendingAmount > 0 {
		amount = sub.PendingAmount
	}
	cy := store.Cycle{ID: ids.New("cyc"), SubscriptionID: sub.ID, PeriodIndex: k, Amount: amount, State: "scheduled",
		CreatedAt: now, UpdatedAt: now}
	if k == 0 {
		cy.NotifiedAt = chargeAt.Add(-billing.MinNoticeGap)
	}
	if err := store.InsertCycle(ctx, tx, cy); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil // already scheduled
		}
		return err
	}
	if k > 0 {
		noticeAt := billing.PreDebitAt(chargeAt, pol.predebitLead)
		if noticeAt.Before(now) {
			noticeAt = now
		}
		if err := s.enqueueNotify(ctx, tx, sub, k, 0, noticeAt); err != nil {
			return err
		}
	}
	if err := s.enqueueCharge(ctx, tx, sub, &cy, pol.snap(m.Method, chargeAt)); err != nil {
		return err
	}
	return store.UpdateCycle(ctx, tx, cy, now)
}

// activate moves an incomplete subscription to active (or trialing) after its
// setup succeeded, and starts the renewal schedule for merchant-triggered
// billing. It is idempotent.
func (s *Service) activate(ctx context.Context, tx store.Tx, sub *store.Subscription) error {
	now := s.nowUTC()
	ev := state.SubSetupSucceeded
	if !sub.TrialEnd.IsZero() && sub.TrialEnd.After(now) {
		ev = state.SubSetupSucceededTrial
	}
	ok, err := s.transition(sub, ev)
	if err != nil || !ok {
		return err
	}
	if sub.BillingMode == string(connector.MerchantTriggered) && sub.TrialEnd.IsZero() && sub.AnchorAt.Before(now) {
		sub.AnchorAt, sub.CurrentPeriodStart = now, now
		sub.CurrentPeriodEnd = billing.Boundary(now, billing.Interval(sub.Interval), sub.IntervalCount, 1)
	}
	sub.CheckoutAction = ""
	if err := s.saveSub(ctx, tx, sub); err != nil {
		return err
	}
	if err := s.emitSub(ctx, tx, EvActivated, *sub, eventData{}); err != nil {
		return err
	}
	if sub.BillingMode == string(connector.MerchantTriggered) {
		return s.scheduleCycle(ctx, tx, *sub, 0)
	}
	return nil
}

// ensureOpenAttempt returns the attempt the next charge will use: the unused
// one if there is one, else a new attempt (on the same payment after a failed
// one), else a new renewal payment with its first attempt.
func (s *Service) ensureOpenAttempt(ctx context.Context, tx store.Tx, sub store.Subscription, cy *store.Cycle) (store.Attempt, error) {
	if cy.AttemptID != "" {
		a, err := store.GetAttempt(ctx, tx, cy.AttemptID)
		if err != nil {
			return store.Attempt{}, err
		}
		if a.Status == string(state.AttemptCreated) {
			return a, nil
		}
	}
	if cy.PaymentID != "" {
		p, err := store.GetPayment(ctx, tx, cy.PaymentID)
		if err != nil {
			return store.Attempt{}, err
		}
		a, err := s.core.AddAttemptTx(ctx, tx, p, sub.ProviderAccountID)
		cy.AttemptID = a.ID
		return a, err
	}
	if cy.PeriodIndex >= 1 && sub.PendingAmount > 0 {
		cy.Amount = sub.PendingAmount // an amount change applies from the next period
	}
	p, a, err := s.core.CreateRenewalPayment(ctx, tx, core.RenewalInput{OrgID: sub.OrgID, AppID: sub.AppID, Mode: sub.Mode,
		SubscriptionID: sub.ID, CustomerID: sub.CustomerID, Description: sub.Description, ReturnURL: sub.ReturnURL,
		Metadata: sub.Metadata, Amount: cy.Amount, Currency: sub.Currency, ProviderAccountID: sub.ProviderAccountID})
	if err != nil {
		return store.Attempt{}, err
	}
	cy.PaymentID, cy.AttemptID = p.ID, a.ID
	return a, nil
}

// ---- pre-debit notice ---------------------------------------------------

func (s *Service) handleNotify(ctx context.Context, raw json.RawMessage) error {
	var pl cyclePayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	var (
		sub     store.Subscription
		cy      store.Cycle
		att     store.Attempt
		m       store.Mandate
		skip    bool
		chargeT time.Time
	)
	err := s.db.Write(ctx, func(tx store.Tx) error {
		var err error
		if sub, err = s.lockSub(ctx, tx, pl.SubscriptionID); err != nil {
			return err
		}
		if cy, err = store.GetCycle(ctx, tx, sub.ID, pl.Period); err != nil {
			skip = errors.Is(err, store.ErrNotFound)
			if skip {
				return nil
			}
			return err
		}
		if terminal(sub.Status) || cy.State == "succeeded" || cy.State == "exhausted" || !cy.NotifiedAt.IsZero() || sub.MandateID == "" ||
			endsBefore(sub, cy) {
			skip = true // nothing will be debited, so there is nothing to announce
			return nil
		}
		if m, err = s.mandateOf(ctx, tx, sub); err != nil {
			return err
		}
		if att, err = s.ensureOpenAttempt(ctx, tx, sub, &cy); err != nil {
			return err
		}
		chargeT = cy.NextRunAt
		return store.UpdateCycle(ctx, tx, cy, s.nowUTC())
	})
	if err != nil || skip {
		return err
	}
	conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
	if err != nil {
		return err
	}
	if mm, ok := conn.(connector.MandateManager); ok {
		err = mm.NotifyPreDebit(ctx, connector.PreDebitRequest{Mandate: connector.MandateRef{ProviderMandateID: m.ProviderMandateID},
			ProviderRef: att.ProviderRef, Amount: cy.Amount, Currency: sub.Currency, DebitDate: chargeT.UnixMilli()})
		if err != nil && !errors.Is(err, connector.ErrUnsupported) {
			return err // retried by the queue
		}
	}
	return s.db.Write(ctx, func(tx store.Tx) error {
		sub, err := s.lockSub(ctx, tx, pl.SubscriptionID)
		if err != nil {
			return err
		}
		cy, err := store.GetCycle(ctx, tx, sub.ID, pl.Period)
		if err != nil || !cy.NotifiedAt.IsZero() {
			return err
		}
		now := s.nowUTC()
		cy.NotifiedAt = now
		if err := store.UpdateCycle(ctx, tx, cy, now); err != nil {
			return err
		}
		ca := chargeT
		return s.emitSub(ctx, tx, EvRenewalUpcoming, sub, eventData{ChargeAt: &ca, RenewalAmount: cy.Amount})
	})
}

// endsBefore reports whether the subscription ends instead of billing this
// cycle: cancel-at-period-end was requested, or all total_count periods are paid.
func endsBefore(sub store.Subscription, cy store.Cycle) bool {
	if cy.PeriodIndex < 1 || cy.PaymentID != "" || cy.Retries != 0 {
		return false
	}
	return sub.CancelAtPeriodEnd || (sub.TotalCount > 0 && sub.RenewalsDone >= sub.TotalCount)
}

// ---- charging -----------------------------------------------------------

func (s *Service) handleCharge(ctx context.Context, raw json.RawMessage) error {
	var pl cyclePayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	type plan struct {
		sub  store.Subscription
		cy   store.Cycle
		att  store.Attempt
		m    store.Mandate
		done bool
	}
	var pn plan
	err := s.db.Write(ctx, func(tx store.Tx) error {
		sub, err := s.lockSub(ctx, tx, pl.SubscriptionID)
		if err != nil {
			return err
		}
		pn.sub = sub
		cy, err := store.GetCycle(ctx, tx, sub.ID, pl.Period)
		if errors.Is(err, store.ErrNotFound) {
			pn.done = true
			return nil
		}
		if err != nil {
			return err
		}
		pn.cy = cy
		pn.done, err = s.prepareCharge(ctx, tx, &pn.sub, &pn.cy, &pn.att, &pn.m)
		return err
	})
	if err != nil || pn.done {
		return err
	}
	return s.executeCharge(ctx, pn.sub, pn.cy, pn.att, pn.m)
}

// prepareCharge decides, under the subscription lock, whether the charge may
// run now and, if so, claims the cycle (state charging with a lease). done is
// true when there is nothing to execute: the step was a no-op, was
// rescheduled, or already failed.
func (s *Service) prepareCharge(ctx context.Context, tx store.Tx, sub *store.Subscription, cy *store.Cycle, att *store.Attempt, m *store.Mandate) (done bool, err error) {
	now := s.nowUTC()
	if terminal(sub.Status) {
		return true, nil
	}
	switch cy.State {
	case "succeeded", "exhausted":
		return true, nil
	case "awaiting":
		return true, nil // the poll job settles it
	case "charging":
		if cy.LeaseUntil.After(now) {
			// Another run is in flight. Look again when its lease ends, so a
			// crashed run is picked up.
			return true, s.deferCharge(ctx, tx, *sub, cy, cy.LeaseUntil)
		}
	}
	pol, err := s.loadPolicy(ctx, tx, sub.AppID)
	if err != nil {
		return false, err
	}
	// End of term (first attempt at a new period only).
	if cy.PeriodIndex >= 1 && cy.PaymentID == "" && cy.Retries == 0 {
		switch {
		case sub.CancelAtPeriodEnd:
			ok, err := s.transition(sub, state.SubPeriodEndCancel)
			if err != nil || !ok {
				return true, err
			}
			cy.State = "exhausted"
			if err := store.UpdateCycle(ctx, tx, *cy, now); err != nil {
				return true, err
			}
			return true, s.afterTermination(ctx, tx, sub, EvCanceled, "cancel_at_period_end")
		case sub.TotalCount > 0 && sub.RenewalsDone >= sub.TotalCount:
			ok, err := s.transition(sub, state.SubCompleted)
			if err != nil || !ok {
				return true, err
			}
			cy.State = "exhausted"
			if err := store.UpdateCycle(ctx, tx, *cy, now); err != nil {
				return true, err
			}
			return true, s.afterTermination(ctx, tx, sub, EvEnded, "completed")
		}
	}
	if *m, err = s.mandateOf(ctx, tx, *sub); err != nil {
		return true, err
	}
	if m.Status != string(state.MandateActive) || (!m.ValidUntil.IsZero() && !now.Before(m.ValidUntil)) {
		return true, s.failCycle(ctx, tx, sub, cy, pol, *m, "mandate_not_active", true)
	}
	// The RBI notice rule: never debit within 24h of the pre-debit notice.
	if cy.NotifiedAt.IsZero() {
		if err := s.enqueueNotify(ctx, tx, *sub, cy.PeriodIndex, cy.Retries, now); err != nil {
			return true, err
		}
		return true, s.deferCharge(ctx, tx, *sub, cy, pol.snap(m.Method, now.Add(billing.MinNoticeGap)))
	}
	if ok, at := billing.ChargeAllowed(cy.NotifiedAt, now); !ok {
		return true, s.deferCharge(ctx, tx, *sub, cy, pol.snap(m.Method, at))
	}
	// NPCI: no UPI AutoPay execution in the peak windows.
	if at := pol.snap(m.Method, now); at.After(now) {
		return true, s.deferCharge(ctx, tx, *sub, cy, at)
	}
	if *att, err = s.ensureOpenAttempt(ctx, tx, *sub, cy); err != nil {
		return true, err
	}
	if cy.Amount > m.MaxAmount {
		return true, s.failCycle(ctx, tx, sub, cy, pol, *m, "amount_exceeds_mandate_max", true)
	}
	cy.State, cy.LeaseUntil = "charging", now.Add(chargeLease)
	return false, store.UpdateCycle(ctx, tx, *cy, now)
}

// deferCharge moves the charge to runAt with a new job.
func (s *Service) deferCharge(ctx context.Context, tx store.Tx, sub store.Subscription, cy *store.Cycle, runAt time.Time) error {
	// The state is left alone: a cycle that is charging elsewhere must stay claimed.
	if err := s.enqueueCharge(ctx, tx, sub, cy, runAt); err != nil {
		return err
	}
	return store.UpdateCycle(ctx, tx, *cy, s.nowUTC())
}

// toSnapshot maps a connector payment snapshot to a core attempt snapshot.
func toSnapshot(ps connector.PaymentSnapshot, amount int64, currency string) core.Snapshot {
	switch ps.Status {
	case connector.PaymentSucceeded:
		a, c := ps.Amount, ps.Currency
		if a == 0 {
			a = amount
		}
		if c == "" {
			c = currency
		}
		return core.Snapshot{Status: state.AttemptCaptured, Amount: a, Currency: c, Method: ps.Method, ProviderPaymentID: ps.ProviderPaymentID}
	case connector.PaymentFailed, connector.PaymentCanceled, connector.PaymentExpired:
		return core.Snapshot{Status: state.AttemptFailed, Method: ps.Method, ProviderPaymentID: ps.ProviderPaymentID,
			FailureCode: ps.ErrorCode, FailureMessage: ps.ErrorMessage}
	case connector.PaymentAuthorized:
		return core.Snapshot{Status: state.AttemptAuthorized, Method: ps.Method, ProviderPaymentID: ps.ProviderPaymentID}
	}
	return core.Snapshot{Status: state.AttemptPending, Method: ps.Method, ProviderPaymentID: ps.ProviderPaymentID}
}

// executeCharge calls the provider (outside any transaction) and settles the
// result. The attempt's provider_ref is the provider-side idempotency token.
func (s *Service) executeCharge(ctx context.Context, sub store.Subscription, cy store.Cycle, att store.Attempt, m store.Mandate) error {
	conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
	if err != nil {
		return s.releaseLease(ctx, sub.ID, cy.PeriodIndex, err)
	}
	mm, ok := conn.(connector.MandateManager)
	if !ok {
		return jobs.Permanent(fmt.Errorf("subscriptions: connector of account %s cannot charge mandates", sub.ProviderAccountID))
	}
	ps, err := mm.ChargeMandate(ctx, connector.ChargeRequest{Mandate: connector.MandateRef{ProviderMandateID: m.ProviderMandateID},
		ProviderRef: att.ProviderRef, Amount: cy.Amount, Currency: sub.Currency})
	var rl connector.ErrRateLimited
	switch {
	case err == nil:
	case errors.Is(err, connector.ErrTemporary), errors.As(err, &rl), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return s.releaseLease(ctx, sub.ID, cy.PeriodIndex, err)
	case errors.Is(err, connector.ErrMandateLimit):
		return s.failLocked(ctx, sub.ID, cy.PeriodIndex, "mandate_limit", true)
	default:
		ps = connector.PaymentSnapshot{Status: connector.PaymentFailed, ErrorCode: "charge_error", ErrorMessage: err.Error()}
	}
	if _, err := s.core.ApplyAttemptSnapshot(ctx, att.ID, toSnapshot(ps, cy.Amount, sub.Currency)); err != nil {
		return s.releaseLease(ctx, sub.ID, cy.PeriodIndex, err)
	}
	return s.reconcile(ctx, sub.ID, cy.ID)
}

// releaseLease puts a claimed cycle back to scheduled (the provider call did
// not complete) and returns cause so the queue retries the job.
func (s *Service) releaseLease(ctx context.Context, subID string, period int, cause error) error {
	err := s.db.Write(ctx, func(tx store.Tx) error {
		if _, err := s.lockSub(ctx, tx, subID); err != nil {
			return err
		}
		cy, err := store.GetCycle(ctx, tx, subID, period)
		if err != nil {
			return err
		}
		if cy.State == "charging" {
			cy.State, cy.LeaseUntil = "scheduled", time.Time{}
			return store.UpdateCycle(ctx, tx, cy, s.nowUTC())
		}
		return nil
	})
	return errors.Join(cause, err)
}

func (s *Service) failLocked(ctx context.Context, subID string, period int, reason string, reauth bool) error {
	return s.db.Write(ctx, func(tx store.Tx) error {
		sub, err := s.lockSub(ctx, tx, subID)
		if err != nil {
			return err
		}
		cy, err := store.GetCycle(ctx, tx, subID, period)
		if err != nil {
			return err
		}
		pol, err := s.loadPolicy(ctx, tx, sub.AppID)
		if err != nil {
			return err
		}
		m, err := s.mandateOf(ctx, tx, sub)
		if err != nil {
			return err
		}
		return s.failCycle(ctx, tx, &sub, &cy, pol, m, reason, reauth)
	})
}

// ---- results ------------------------------------------------------------

// reconcile settles a cycle from the state of its payment: succeeded,
// failed (an attempt failed), or still waiting on the provider. It is
// idempotent and safe to call from anywhere.
func (s *Service) reconcile(ctx context.Context, subID, cycleID string) error {
	return s.db.Write(ctx, func(tx store.Tx) error {
		sub, err := s.lockSub(ctx, tx, subID)
		if err != nil {
			return err
		}
		cys, err := store.ListCycles(ctx, tx, subID)
		if err != nil {
			return err
		}
		var cy store.Cycle
		for _, c := range cys {
			if c.ID == cycleID {
				cy = c
			}
		}
		if cy.ID == "" || cy.PaymentID == "" || cy.State == "succeeded" || cy.State == "exhausted" {
			return nil
		}
		p, err := store.GetPayment(ctx, tx, cy.PaymentID)
		if err != nil {
			return err
		}
		a, err := store.GetAttempt(ctx, tx, cy.AttemptID)
		if err != nil {
			return err
		}
		now := s.nowUTC()
		switch {
		case p.Status == string(state.PaymentSucceeded):
			return s.succeedCycle(ctx, tx, &sub, &cy)
		case a.Status == string(state.AttemptFailed) && cy.PeriodIndex >= 0 && (cy.State == "charging" || cy.State == "awaiting"):
			pol, err := s.loadPolicy(ctx, tx, sub.AppID)
			if err != nil {
				return err
			}
			m, err := s.mandateOf(ctx, tx, sub)
			if err != nil {
				return err
			}
			return s.failCycle(ctx, tx, &sub, &cy, pol, m, firstNonEmpty(a.FailureCode, "charge_failed"), false)
		case cy.State == "charging" || cy.State == "scheduled":
			cy.State, cy.LeaseUntil = "awaiting", time.Time{}
			if cy.PeriodIndex >= 0 {
				cy.JobSeq++
				if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobPollRenewal, RunAt: now.Add(pollInterval),
					Payload:   cyclePayload{SubscriptionID: sub.ID, Period: cy.PeriodIndex},
					DedupeKey: fmt.Sprintf("sub:%s:%d:poll:%d", sub.ID, cy.PeriodIndex, cy.JobSeq)}); err != nil {
					return err
				}
			}
			return store.UpdateCycle(ctx, tx, cy, now)
		}
		return nil
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ReconcilePayment settles the subscription cycle of a renewal payment whose
// attempt changed outside this package (for example a provider webhook the
// inbound processing applied). It is a no-op for other payments.
func (s *Service) ReconcilePayment(ctx context.Context, paymentID string) error {
	var cy store.Cycle
	err := s.db.Read(ctx, func(q store.Querier) (err error) { cy, err = store.GetCycleByPayment(ctx, q, paymentID); return })
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.reconcile(ctx, cy.SubscriptionID, cy.ID)
}

// succeedCycle books a paid renewal: bumps renewals_done, applies a pending
// amount, advances the period, returns the subscription to active, resets
// dunning and (merchant-triggered) schedules the next period.
func (s *Service) succeedCycle(ctx context.Context, tx store.Tx, sub *store.Subscription, cy *store.Cycle) error {
	now := s.nowUTC()
	cy.State, cy.LeaseUntil = "succeeded", time.Time{}
	if err := store.UpdateCycle(ctx, tx, *cy, now); err != nil {
		return err
	}
	if terminal(sub.Status) {
		return nil // paid after the end: the payment machinery flags it, nothing to bill
	}
	sub.RenewalsDone++
	if cy.Amount != sub.Amount {
		sub.Amount = cy.Amount
	}
	if sub.PendingAmount == cy.Amount {
		sub.PendingAmount = 0
	}
	sub.DunningAttempt, sub.NextRetryAt = 0, time.Time{}
	if cy.PeriodIndex >= 0 {
		iv := billing.Interval(sub.Interval)
		sub.CurrentPeriodStart = billing.Boundary(sub.AnchorAt, iv, sub.IntervalCount, cy.PeriodIndex)
		sub.CurrentPeriodEnd = billing.Boundary(sub.AnchorAt, iv, sub.IntervalCount, cy.PeriodIndex+1)
	}
	if _, err := s.transition(sub, state.SubRenewalSucceeded); err != nil {
		return err
	}
	if err := s.saveSub(ctx, tx, sub); err != nil {
		return err
	}
	if err := s.emitSub(ctx, tx, EvRenewed, *sub, eventData{RenewalAmount: cy.Amount}); err != nil {
		return err
	}
	if sub.BillingMode == string(connector.MerchantTriggered) && cy.PeriodIndex >= 0 {
		return s.scheduleCycle(ctx, tx, *sub, cy.PeriodIndex+1)
	}
	return nil
}

// failCycle records a failed renewal: the subscription goes past_due and the
// next dunning retry is scheduled from dunning_schedule (UPI capped at 3
// retries). When the retries are used up, dunning_final_action applies.
// reauth marks failures only a new mandate can fix: no retry is scheduled, a
// re-authorization is requested instead and the charge resumes once it is done.
func (s *Service) failCycle(ctx context.Context, tx store.Tx, sub *store.Subscription, cy *store.Cycle, pol policy, m store.Mandate, reason string, reauth bool) error {
	now := s.nowUTC()
	cy.State, cy.LeaseUntil = "failed", time.Time{}
	was := sub.Status
	if _, err := s.transition(sub, state.SubRenewalFailed); err != nil {
		return err
	}
	pastDue := was != sub.Status && sub.Status == string(state.SubPastDue)
	if reauth {
		if err := store.UpdateCycle(ctx, tx, *cy, now); err != nil {
			return err
		}
		if err := s.saveSub(ctx, tx, sub); err != nil {
			return err
		}
		if pastDue {
			if err := s.emitSub(ctx, tx, EvPastDue, *sub, eventData{Reason: reason}); err != nil {
				return err
			}
		}
		if sub.RequiresReauthorization && sub.PendingMandateID != "" {
			return nil // a re-authorization is already under way
		}
		_, err := s.requestReauthorization(ctx, tx, sub, pol, reason)
		return err
	}
	sched := billing.RetrySchedule(pol.dunning, billing.Method(m.Method))
	if cy.Retries < len(sched) {
		runAt := pol.snap(m.Method, now.Add(sched[cy.Retries]))
		cy.Retries++
		cy.State = "scheduled"
		sub.DunningAttempt, sub.NextRetryAt = cy.Retries, runAt
		conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
		if err != nil {
			return err
		}
		if conn.Info().PreDebitPerAttempt {
			cy.NotifiedAt = time.Time{} // each retry needs its own notice
			noticeAt := billing.PreDebitAt(runAt, pol.predebitLead)
			if noticeAt.Before(now) {
				noticeAt = now
			}
			if err := s.enqueueNotify(ctx, tx, *sub, cy.PeriodIndex, cy.Retries, noticeAt); err != nil {
				return err
			}
		}
		if err := s.enqueueCharge(ctx, tx, *sub, cy, runAt); err != nil {
			return err
		}
	} else {
		cy.State = "exhausted"
		sub.NextRetryAt = time.Time{}
	}
	if err := store.UpdateCycle(ctx, tx, *cy, now); err != nil {
		return err
	}
	if cy.State == "exhausted" && pol.finalAction == "cancel" {
		if ok, err := s.transition(sub, state.SubDunningExhaustedCancel); err != nil {
			return err
		} else if ok {
			return s.afterTermination(ctx, tx, sub, EvCanceled, "dunning_exhausted")
		}
	}
	if err := s.saveSub(ctx, tx, sub); err != nil {
		return err
	}
	if pastDue {
		return s.emitSub(ctx, tx, EvPastDue, *sub, eventData{Reason: reason})
	}
	return nil
}

// handlePoll fetches the provider's view of an awaiting renewal.
func (s *Service) handlePoll(ctx context.Context, raw json.RawMessage) error {
	var pl cyclePayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return jobs.Permanent(err)
	}
	var sub store.Subscription
	var cy store.Cycle
	var att store.Attempt
	skip := false
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if sub, err = store.GetSubscription(ctx, q, pl.SubscriptionID); err != nil {
			return err
		}
		if cy, err = store.GetCycle(ctx, q, sub.ID, pl.Period); err != nil {
			return err
		}
		if cy.State != "awaiting" {
			skip = true
			return nil
		}
		att, err = store.GetAttempt(ctx, q, cy.AttemptID)
		return err
	})
	if err != nil || skip {
		return err
	}
	conn, err := s.connectors.Get(ctx, sub.ProviderAccountID)
	if err != nil {
		return err
	}
	ps, err := conn.FetchPayment(ctx, connector.PaymentRef{ProviderRef: att.ProviderRef, ProviderPaymentID: att.ProviderPaymentID})
	switch {
	case errors.Is(err, connector.ErrNotFound):
		ps = connector.PaymentSnapshot{Status: connector.PaymentFailed, ErrorCode: "not_found", ErrorMessage: "the provider has no such debit"}
	case err != nil:
		return err
	}
	snap := toSnapshot(ps, cy.Amount, sub.Currency)
	if snap.Status == state.AttemptPending && cy.JobSeq >= maxPolls {
		snap = core.Snapshot{Status: state.AttemptFailed, FailureCode: "timeout", FailureMessage: "no result from the provider"}
	}
	if _, err := s.core.ApplyAttemptSnapshot(ctx, att.ID, snap); err != nil {
		return err
	}
	if snap.Status == state.AttemptPending || snap.Status == state.AttemptAuthorized {
		// Still waiting: queue the next poll.
		return s.db.Write(ctx, func(tx store.Tx) error {
			sub, err := s.lockSub(ctx, tx, pl.SubscriptionID)
			if err != nil {
				return err
			}
			c, err := store.GetCycle(ctx, tx, sub.ID, pl.Period)
			if err != nil || c.State != "awaiting" {
				return err
			}
			c.JobSeq++
			if err := s.jobs.Enqueue(ctx, tx, jobs.Job{Kind: JobPollRenewal, RunAt: s.nowUTC().Add(pollInterval),
				Payload:   cyclePayload{SubscriptionID: sub.ID, Period: c.PeriodIndex},
				DedupeKey: fmt.Sprintf("sub:%s:%d:poll:%d", sub.ID, c.PeriodIndex, c.JobSeq)}); err != nil {
				return err
			}
			return store.UpdateCycle(ctx, tx, c, s.nowUTC())
		})
	}
	return s.reconcile(ctx, sub.ID, cy.ID)
}
