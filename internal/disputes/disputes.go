// Package disputes implements spec section 6.7: disputes arrive from
// webhooks, polling or manual entry, move through the dispute state machine,
// hold and release money in the ledger, flag the payment, emit events and
// raise deadline alerts. Operators can accept a dispute, submit evidence and
// ask for a credit note where the connector or the invoicing lane supports it.
package disputes

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
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/ids"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/pkg/connector"
)

// ErrUnknownPayment means no attempt of the delivering account has the
// dispute's provider payment id, so the dispute cannot be attached.
var ErrUnknownPayment = errors.New("disputes: no payment of this provider account matches the dispute")

// ErrNoReader means the account's connector cannot read disputes.
var ErrNoReader = errors.New("disputes: the connector cannot read disputes")

// Payment flags set by disputes.
const (
	FlagDisputed    = "disputed"
	FlagDisputeLost = "dispute_lost"
)

// Alerter raises ops alerts (*notify.Alerts).
type Alerter interface {
	Raise(ctx context.Context, al notify.Alert) error
}

// CreditNoter issues the credit note of a dispute (*invoice.Service).
type CreditNoter interface {
	IssueDisputeCreditNote(ctx context.Context, tx store.Tx, d store.Dispute, p store.Payment, a store.Attempt) (*store.CreditNote, error)
}

// ErrCreditCutoff is returned by a CreditNoter whose deadline passed.
var ErrCreditCutoff = errors.New("credit note deadline passed")

// Options configures a Service.
type Options struct {
	DB       *store.DB
	Settings *settings.Service
	Emitter  events.Emitter
	Alerts   Alerter     // nil disables alerts
	Credit   CreditNoter // nil disables credit notes
	Conns    *connectors.Service
	Logger   *slog.Logger
	Now      func() time.Time
	// CutoffErr, when set, is the error value the CreditNoter returns for a
	// passed deadline (default: invoice's, matched by message via errors.Is).
	CutoffErr error
}

// Service manages disputes.
type Service struct{ o Options }

// New builds a Service.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Emitter == nil {
		o.Emitter = core.NoopEmitter
	}
	return &Service{o: o}
}

func (s *Service) now() time.Time { return s.o.Now().UTC() }

func invalid(format string, a ...any) error { return core.NewError(core.ErrInvalid, format, a...) }

// Result reports what Apply did.
type Result struct {
	Dispute store.Dispute
	Created bool
	Changed bool
}

// DisputeView is the public JSON shape of a dispute, used in events.
type DisputeView struct {
	ID        string    `json:"id"`
	Object    string    `json:"object"`
	Livemode  bool      `json:"livemode"` // false for a test-mode object
	PaymentID string    `json:"payment_id"`
	AttemptID string    `json:"attempt_id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Reason    string    `json:"reason,omitempty"`
	Status    string    `json:"status"`
	Stage     string    `json:"stage,omitempty"`
	DueBy     time.Time `json:"due_by,omitzero"`
	Source    string    `json:"source"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// ViewDispute renders d.
func ViewDispute(d store.Dispute) DisputeView {
	return DisputeView{ID: d.ID, Object: "dispute", Livemode: d.Mode == "live", PaymentID: d.PaymentID, AttemptID: d.AttemptID, Amount: d.Amount, Currency: d.Currency,
		Reason: d.Reason, Status: d.Status, Stage: d.Stage, DueBy: d.DueBy, Source: d.Source, Version: d.Version, CreatedAt: d.CreatedAt}
}

// alertTask is an alert to deliver once, after the transaction commits.
type alertTask struct {
	disputeID, column string
	alert             notify.Alert
}

func addFlag(p *store.Payment, f string) {
	if !p.HasFlag(f) {
		p.Flags = append(p.Flags, f)
	}
}

func dropFlag(p *store.Payment, f string) {
	p.Flags = slices.DeleteFunc(p.Flags, func(x string) bool { return x == f })
}

// Apply upserts the dispute described by snap for the delivering account and
// moves it through the state machine. The snapshot must come from the
// provider's API (or an authenticated poll), never from an unverified
// webhook. ErrUnknownPayment means the payment id matches none of the
// account's attempts.
func (s *Service) Apply(ctx context.Context, accountID string, snap connector.DisputeSnapshot, source string) (Result, error) {
	var res Result
	var tasks []alertTask
	err := s.o.DB.Write(ctx, func(tx store.Tx) error {
		res, tasks = Result{}, nil
		a, err := store.FindAttempt(ctx, tx, accountID, "", "", snap.ProviderPaymentID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrUnknownPayment
		}
		if err != nil {
			return err
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, a.PaymentID)
		if err != nil {
			return err
		}
		res, tasks, err = s.upsert(ctx, tx, a, p, snap, source)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	s.deliver(ctx, tasks)
	return res, nil
}

// upsert creates or advances the dispute inside tx. The payment is locked.
func (s *Service) upsert(ctx context.Context, tx store.Tx, a store.Attempt, p store.Payment, snap connector.DisputeSnapshot, source string) (Result, []alertTask, error) {
	var res Result
	var tasks []alertTask
	target := state.DisputeStatus(snap.Status)
	if !slices.Contains(state.AllDisputeStatuses, target) {
		return res, nil, invalid("unknown dispute status %q", snap.Status)
	}
	// A dispute holds money back from this payment: it cannot be for more than
	// we received, nor in another currency.
	if snap.Amount > a.AmountReceived || (snap.Currency != "" && a.CurrencyReceived != "" && !strings.EqualFold(snap.Currency, a.CurrencyReceived)) {
		return res, nil, invalid("dispute %s (%d %s) cannot be of payment %s (%d %s received)", snap.ProviderDisputeID,
			snap.Amount, snap.Currency, p.ID, a.AmountReceived, a.CurrencyReceived)
	}
	now := s.now()
	d, err := store.FindDispute(ctx, tx, a.ProviderAccountID, snap.ProviderDisputeID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		d = store.Dispute{ID: ids.New("dsp"), OrgID: p.OrgID, AppID: p.AppID, Mode: p.Mode, PaymentID: p.ID, AttemptID: a.ID,
			ProviderAccountID: a.ProviderAccountID, ProviderDisputeID: snap.ProviderDisputeID, Amount: snap.Amount,
			Currency: snap.Currency, Reason: snap.Reason, Status: string(state.DisputeNeedsResponse), Stage: snap.Phase,
			Source: source, Evidence: "{}", Version: 1, CreatedAt: now, UpdatedAt: now}
		if snap.DueBy > 0 {
			d.DueBy = time.UnixMilli(snap.DueBy).UTC()
		}
		if d.Amount <= 0 {
			d.Amount = a.AmountReceived
		}
		if d.Currency == "" {
			d.Currency = a.CurrencyReceived
		}
		if err := store.InsertDispute(ctx, tx, d); err != nil {
			return res, nil, err
		}
		if err := s.ledger(ctx, tx, d, "dispute_hold", -d.Amount, now); err != nil {
			return res, nil, err
		}
		addFlag(&p, FlagDisputed)
		if err := store.UpdatePayment(ctx, tx, &p, now); err != nil {
			return res, nil, err
		}
		if err := s.emit(ctx, tx, "dispute.opened", d); err != nil {
			return res, nil, err
		}
		tasks = append(tasks, alertTask{d.ID, "alert_open_at", openedAlert(d)})
		res.Created, res.Changed = true, true
	case err != nil:
		return res, nil, err
	}

	switch state.DisputeNext(state.DisputeStatus(d.Status), target) {
	case state.Reject:
		return res, nil, core.NewError(core.ErrInvalidState, "dispute %s cannot move from %s to %s", d.ID, d.Status, target)
	case state.Apply:
		more, err := s.transition(ctx, tx, &d, &p, a, target, snap, now)
		if err != nil {
			return res, nil, err
		}
		tasks = append(tasks, more...)
		res.Changed = true
	default:
		// Same or stale status: refresh descriptive fields of an open dispute.
		if !res.Created && !state.DisputeStatus(d.Status).Terminal() && s.mergeFields(&d, snap) {
			if err := store.UpdateDispute(ctx, tx, &d, now); err != nil {
				return res, nil, err
			}
			if err := s.emit(ctx, tx, "dispute.updated", d); err != nil {
				return res, nil, err
			}
			res.Changed = true
		}
	}
	res.Dispute = d
	return res, tasks, nil
}

// mergeFields copies the due date, reason and stage from snap and reports
// whether anything changed.
func (s *Service) mergeFields(d *store.Dispute, snap connector.DisputeSnapshot) bool {
	changed := false
	if snap.DueBy > 0 {
		if due := time.UnixMilli(snap.DueBy).UTC(); !due.Equal(d.DueBy) {
			d.DueBy, changed = due, true
		}
	}
	if snap.Reason != "" && snap.Reason != d.Reason {
		d.Reason, changed = snap.Reason, true
	}
	if snap.Phase != "" && snap.Phase != d.Stage {
		d.Stage, changed = snap.Phase, true
	}
	return changed
}

func (s *Service) transition(ctx context.Context, tx store.Tx, d *store.Dispute, p *store.Payment, a store.Attempt,
	to state.DisputeStatus, snap connector.DisputeSnapshot, now time.Time) ([]alertTask, error) {
	var tasks []alertTask
	s.mergeFields(d, snap)
	d.Status = string(to)
	if err := store.UpdateDispute(ctx, tx, d, now); err != nil {
		return nil, err
	}
	evType := "dispute.updated"
	switch to {
	case state.DisputeWon, state.DisputeLost, state.DisputeAccepted, state.DisputeClosed:
		if err := s.ledger(ctx, tx, *d, "dispute_release", d.Amount, now); err != nil {
			return nil, err
		}
		// Another open dispute on this payment still holds money back: the flag
		// (which blocks refunds of ours) stays until the last one ends.
		open, err := store.CountOtherOpenDisputes(ctx, tx, p.ID, d.ID)
		if err != nil {
			return nil, err
		}
		if open == 0 {
			dropFlag(p, FlagDisputed)
		}
		switch to {
		case state.DisputeWon:
			evType = "dispute.won"
		case state.DisputeLost, state.DisputeAccepted:
			evType = "dispute.lost"
			if err := s.ledger(ctx, tx, *d, "dispute_loss", -d.Amount, now); err != nil {
				return nil, err
			}
			addFlag(p, FlagDisputeLost)
			tasks = append(tasks, alertTask{d.ID, "alert_lost_at", lostAlert(*d)})
			if err := s.creditOnLoss(ctx, tx, *d, *p, a); err != nil {
				return nil, err
			}
		}
		if err := store.UpdatePayment(ctx, tx, p, now); err != nil {
			return nil, err
		}
	}
	return tasks, s.emit(ctx, tx, evType, *d)
}

// creditOnLoss issues the credit note when dispute_credit_note is on_loss.
func (s *Service) creditOnLoss(ctx context.Context, tx store.Tx, d store.Dispute, p store.Payment, a store.Attempt) error {
	if s.o.Credit == nil {
		return nil
	}
	v, err := settings.ResolveWith(ctx, tx, p.AppID, "dispute_credit_note")
	if err != nil {
		return err
	}
	var mode string
	if err := v.Decode(&mode); err != nil || mode != "on_loss" {
		return err
	}
	if _, err := s.o.Credit.IssueDisputeCreditNote(ctx, tx, d, p, a); err != nil && !s.isCutoff(err) {
		return err
	} else if err != nil {
		s.o.Logger.Warn("disputes: credit note deadline passed; issue it manually if needed", "dispute", d.ID)
	}
	return nil
}

func (s *Service) isCutoff(err error) bool {
	return s.o.CutoffErr != nil && errors.Is(err, s.o.CutoffErr)
}

func (s *Service) ledger(ctx context.Context, tx store.Tx, d store.Dispute, kind string, amount int64, now time.Time) error {
	return store.InsertLedgerEntry(ctx, tx, store.LedgerEntry{ID: ids.New("led"), OrgID: d.OrgID, Mode: d.Mode, Kind: kind,
		Amount: amount, Currency: d.Currency, PaymentID: d.PaymentID, DisputeID: d.ID, AttemptID: d.AttemptID, OccurredAt: now})
}

func (s *Service) emit(ctx context.Context, tx store.Tx, typ string, d store.Dispute) error {
	return s.o.Emitter.Emit(ctx, tx, events.Event{OrgID: d.OrgID, AppID: d.AppID, Mode: d.Mode, Type: typ, ObjectType: "dispute",
		ObjectID: d.ID, ObjectVersion: d.Version, Data: ViewDispute(d)})
}

// ---- alerts ----

func openedAlert(d store.Dispute) notify.Alert {
	msg := fmt.Sprintf("A dispute of %s %s was opened on payment %s", money(d.Amount), d.Currency, d.PaymentID)
	if !d.DueBy.IsZero() {
		msg += ". Respond by " + d.DueBy.Format("2006-01-02 15:04 MST")
	}
	return notify.Alert{Severity: notify.SeverityWarning, Kind: "dispute.opened", Message: msg, Refs: refs(d)}
}

func lostAlert(d store.Dispute) notify.Alert {
	return notify.Alert{Severity: notify.SeverityCritical, Kind: "dispute.lost",
		Message: fmt.Sprintf("Dispute %s on payment %s was lost (%s %s)", d.ID, d.PaymentID, money(d.Amount), d.Currency), Refs: refs(d)}
}

func dueAlert(d store.Dispute, hours int, sev notify.Severity) notify.Alert {
	return notify.Alert{Severity: sev, Kind: fmt.Sprintf("dispute.due_%dh", hours),
		Message: fmt.Sprintf("Dispute %s on payment %s is due within %d hours (%s)", d.ID, d.PaymentID, hours, d.DueBy.Format("2006-01-02 15:04 MST")),
		Refs:    refs(d)}
}

func refs(d store.Dispute) map[string]string {
	return map[string]string{"dispute": d.ID, "payment": d.PaymentID, "app": d.AppID, "provider_account": d.ProviderAccountID}
}

func money(minor int64) string { return fmt.Sprintf("%d.%02d", minor/100, minor%100) }

// deliver sends alerts once each: the alert's column is claimed first, and
// released again if no channel took the alert, so the next run retries.
func (s *Service) deliver(ctx context.Context, tasks []alertTask) {
	for _, t := range tasks {
		s.alertOnce(ctx, t)
	}
}

func (s *Service) alertOnce(ctx context.Context, t alertTask) {
	if s.o.Alerts == nil {
		return
	}
	var claimed bool
	err := s.o.DB.Write(ctx, func(tx store.Tx) (err error) {
		claimed, err = store.ClaimDisputeAlert(ctx, tx, t.disputeID, t.column, s.now())
		return
	})
	if err != nil || !claimed {
		if err != nil {
			s.o.Logger.Warn("disputes: claiming alert", "dispute", t.disputeID, "err", err)
		}
		return
	}
	if err := s.o.Alerts.Raise(ctx, t.alert); err != nil {
		s.o.Logger.Warn("disputes: alert not delivered; will retry", "dispute", t.disputeID, "kind", t.alert.Kind, "err", err)
		_ = s.o.DB.Write(ctx, func(tx store.Tx) error { return store.ReleaseDisputeAlert(ctx, tx, t.disputeID, t.column) })
	}
}

// CheckDeadlines raises the deadline alerts of open disputes once each: the
// opening alert (retried if it failed), 72 hours before the deadline and 24
// hours before it. Resolved disputes are skipped.
func (s *Service) CheckDeadlines(ctx context.Context) error {
	var open []store.Dispute
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { open, err = store.ListOpenDisputes(ctx, q, 5000); return })
	if err != nil {
		return err
	}
	now := s.now()
	for _, d := range open {
		s.alertOnce(ctx, alertTask{d.ID, "alert_open_at", openedAlert(d)})
		if d.DueBy.IsZero() {
			continue
		}
		left := d.DueBy.Sub(now)
		switch {
		case left <= 24*time.Hour:
			// Past the 72h mark as well: record it without a second alert.
			s.silence(ctx, d.ID, "alert_72h_at")
			s.alertOnce(ctx, alertTask{d.ID, "alert_24h_at", dueAlert(d, 24, notify.SeverityCritical)})
		case left <= 72*time.Hour:
			s.alertOnce(ctx, alertTask{d.ID, "alert_72h_at", dueAlert(d, 72, notify.SeverityWarning)})
		}
	}
	return nil
}

func (s *Service) silence(ctx context.Context, id, column string) {
	_ = s.o.DB.Write(ctx, func(tx store.Tx) error {
		_, err := store.ClaimDisputeAlert(ctx, tx, id, column, s.now())
		return err
	})
}

// ---- reading from providers ----

func (s *Service) reader(ctx context.Context, accountID string) (connector.DisputeReader, connector.Connector, error) {
	live, err := s.o.Conns.Get(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	r, ok := live.Connector.(connector.DisputeReader)
	if !ok {
		return nil, live.Connector, ErrNoReader
	}
	return r, live.Connector, nil
}

// SyncNotification handles a dispute notification from an inbound webhook.
// The notification is only a hint: the dispute is fetched from the provider
// and that snapshot is applied.
func (s *Service) SyncNotification(ctx context.Context, accountID string, n connector.Notification) (Result, error) {
	if n.Dispute == nil || n.Dispute.ProviderDisputeID == "" {
		return Result{}, invalid("dispute notification without a dispute id")
	}
	r, _, err := s.reader(ctx, accountID)
	if err != nil {
		return Result{}, err
	}
	snap, err := r.FetchDispute(ctx, connector.DisputeRef{ProviderDisputeID: n.Dispute.ProviderDisputeID})
	if err != nil {
		return Result{}, fmt.Errorf("disputes: fetching %s: %w", n.Dispute.ProviderDisputeID, err)
	}
	return s.Apply(ctx, accountID, snap, "webhook")
}

const pollWindow = 45 * 24 * time.Hour

// Poll lists the recent disputes of every account whose connector can read
// them and applies each one. Provider errors and disputes of payments Paymux
// does not know are logged and skipped.
func (s *Service) Poll(ctx context.Context) error {
	var accounts []store.ProviderAccount
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { accounts, err = store.ListActiveProviderAccounts(ctx, q); return })
	if err != nil {
		return err
	}
	now := s.now()
	for _, pa := range accounts {
		r, _, err := s.reader(ctx, pa.ID)
		if err != nil {
			if !errors.Is(err, ErrNoReader) {
				s.o.Logger.Warn("disputes: poll: connector unavailable", "account", pa.ID, "err", err)
			}
			continue
		}
		cursor := connector.Cursor("")
		for page := 0; page < 50; page++ {
			pg, err := r.ListDisputes(ctx, connector.TimeRange{From: now.Add(-pollWindow).UnixMilli(), To: now.UnixMilli()}, cursor)
			if err != nil {
				s.o.Logger.Warn("disputes: poll: listing", "account", pa.ID, "err", err)
				break
			}
			for _, snap := range pg.Items {
				if _, err := s.Apply(ctx, pa.ID, snap, "poll"); err != nil && !errors.Is(err, ErrUnknownPayment) {
					s.o.Logger.Warn("disputes: poll: applying", "account", pa.ID, "dispute", snap.ProviderDisputeID, "err", err)
				}
			}
			if pg.Next == "" {
				break
			}
			cursor = pg.Next
		}
	}
	return nil
}

// ---- operator actions ----

func (s *Service) load(ctx context.Context, q store.Queryer, sc core.OrgScope, id string) (store.Dispute, error) {
	d, err := store.GetDispute(ctx, q, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && (sc.OrgID == "" || d.OrgID != sc.OrgID) {
		return store.Dispute{}, core.NewError(core.ErrNotFound, "dispute not found")
	}
	return d, err
}

// Get returns a dispute of the scope's org.
func (s *Service) Get(ctx context.Context, sc core.OrgScope, id string) (store.Dispute, error) {
	var d store.Dispute
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { d, err = s.load(ctx, q, sc, id); return })
	return d, err
}

// List returns the org's disputes, soonest deadline first.
func (s *Service) List(ctx context.Context, sc core.OrgScope, openOnly bool) ([]store.Dispute, error) {
	if sc.OrgID == "" {
		return nil, invalid("an org scope is required")
	}
	var l []store.Dispute
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		l, err = store.ListDisputes(ctx, q, sc.OrgID, openOnly, 500)
		return
	})
	return l, err
}

// ManualInput describes a dispute entered by hand (Zoho, or one a provider
// never reported).
type ManualInput struct {
	PaymentID string
	Amount    int64
	Reason    string
	Stage     string
	DueBy     time.Time
}

// CreateManual records a dispute for a payment of the scope's org, against
// its succeeded attempt, and audits it.
func (s *Service) CreateManual(ctx context.Context, actor audit.Actor, sc core.OrgScope, in ManualInput) (store.Dispute, error) {
	if sc.OrgID == "" {
		return store.Dispute{}, invalid("an org scope is required")
	}
	if in.Amount <= 0 {
		return store.Dispute{}, invalid("amount must be positive")
	}
	var out store.Dispute
	var tasks []alertTask
	err := s.o.DB.Write(ctx, func(tx store.Tx) error {
		p, err := store.GetPaymentForUpdate(ctx, tx, in.PaymentID)
		if errors.Is(err, store.ErrNotFound) || err == nil && p.OrgID != sc.OrgID {
			return core.NewError(core.ErrNotFound, "payment not found")
		}
		if err != nil {
			return err
		}
		attempts, err := store.ListAttempts(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		var a *store.Attempt
		for i := range attempts {
			if attempts[i].Status == string(state.AttemptCaptured) && !attempts[i].Duplicate {
				a = &attempts[i]
				break
			}
		}
		if a == nil {
			return invalid("the payment has no succeeded attempt to dispute")
		}
		if in.Amount > a.AmountReceived {
			return invalid("amount exceeds the %d received", a.AmountReceived)
		}
		snap := connector.DisputeSnapshot{ProviderDisputeID: "manual_" + ids.New("dsp"), Status: connector.DisputeNeedsResponse,
			RawStatus: "manual", Phase: in.Stage, Reason: in.Reason, Amount: in.Amount, Currency: a.CurrencyReceived}
		if !in.DueBy.IsZero() {
			snap.DueBy = in.DueBy.UnixMilli()
		}
		res, t, err := s.upsert(ctx, tx, *a, p, snap, "manual")
		if err != nil {
			return err
		}
		out, tasks = res.Dispute, t
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "dispute.create_manual", Target: out.ID,
			After: ViewDispute(out)})
	})
	if err != nil {
		return store.Dispute{}, err
	}
	s.deliver(ctx, tasks)
	return out, nil
}

// SetManualStatus moves a manually entered dispute to a new status (for
// example won or lost once the provider's portal says so).
func (s *Service) SetManualStatus(ctx context.Context, actor audit.Actor, sc core.OrgScope, id string, to state.DisputeStatus) (store.Dispute, error) {
	var out store.Dispute
	var tasks []alertTask
	err := s.o.DB.Write(ctx, func(tx store.Tx) error {
		d, err := s.load(ctx, tx, sc, id)
		if err != nil {
			return err
		}
		if d.Source != "manual" {
			return invalid("only manually entered disputes are edited by hand; this one is kept in sync with the provider")
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, d.PaymentID)
		if err != nil {
			return err
		}
		a, err := store.GetAttempt(ctx, tx, d.AttemptID)
		if err != nil {
			return err
		}
		before := d.Status
		res, t, err := s.upsert(ctx, tx, a, p, connector.DisputeSnapshot{ProviderDisputeID: d.ProviderDisputeID, Status: connector.DisputeStatus(to)}, "manual")
		if err != nil {
			return err
		}
		out, tasks = res.Dispute, t
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "dispute.set_status", Target: id,
			Before: map[string]string{"status": before}, After: map[string]string{"status": out.Status}})
	})
	if err != nil {
		return store.Dispute{}, err
	}
	s.deliver(ctx, tasks)
	return out, nil
}

func (s *Service) open(d store.Dispute) error {
	if state.DisputeStatus(d.Status).Terminal() {
		return core.NewError(core.ErrInvalidState, "dispute %s is already %s", d.ID, d.Status)
	}
	if d.Source == "manual" {
		return invalid("this dispute was entered by hand: respond in the provider's portal and set its status here")
	}
	return nil
}

// refresh re-reads the dispute at the provider and applies it.
func (s *Service) refresh(ctx context.Context, d store.Dispute) (store.Dispute, error) {
	r, _, err := s.reader(ctx, d.ProviderAccountID)
	if err != nil {
		return d, nil // no reader: the poller or a webhook will catch up
	}
	snap, err := r.FetchDispute(ctx, connector.DisputeRef{ProviderDisputeID: d.ProviderDisputeID})
	if err != nil {
		s.o.Logger.Warn("disputes: refresh after action", "dispute", d.ID, "err", err)
		return d, nil
	}
	res, err := s.Apply(ctx, d.ProviderAccountID, snap, "poll")
	if err != nil {
		return d, err
	}
	return res.Dispute, nil
}

func (s *Service) responder(ctx context.Context, d store.Dispute) (connector.DisputeResponder, error) {
	live, err := s.o.Conns.Get(ctx, d.ProviderAccountID)
	if err != nil {
		return nil, err
	}
	r, ok := live.Connector.(connector.DisputeResponder)
	if !ok {
		return nil, invalid("this provider's connector cannot respond to disputes; use the provider's portal")
	}
	return r, nil
}

// Accept accepts the dispute at the provider (conceding it) where the
// connector supports it, then re-reads the provider's state.
func (s *Service) Accept(ctx context.Context, actor audit.Actor, sc core.OrgScope, id string) (store.Dispute, error) {
	d, err := s.Get(ctx, sc, id)
	if err != nil {
		return store.Dispute{}, err
	}
	if err := s.open(d); err != nil {
		return store.Dispute{}, err
	}
	r, err := s.responder(ctx, d)
	if err != nil {
		return store.Dispute{}, err
	}
	if err := r.AcceptDispute(ctx, connector.DisputeRef{ProviderDisputeID: d.ProviderDisputeID}); err != nil {
		return store.Dispute{}, fmt.Errorf("disputes: provider refused to accept: %w", err)
	}
	if err := s.auditAction(ctx, actor, sc, "dispute.accept", d.ID, nil); err != nil {
		return store.Dispute{}, err
	}
	return s.refresh(ctx, d)
}

func (s *Service) auditAction(ctx context.Context, actor audit.Actor, sc core.OrgScope, action, target string, after any) error {
	return s.o.DB.Write(ctx, func(tx store.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: action, Target: target, After: after})
	})
}

// EvidenceFile is one uploaded evidence document.
type EvidenceFile struct {
	Type        string // the provider's evidence type, e.g. shipping_proof
	ContentType string // application/pdf, image/jpeg or image/png
	Data        []byte
}

// EvidenceInput is the evidence an operator submits.
type EvidenceInput struct {
	Text  string
	Files []EvidenceFile
}

// MaxEvidenceBytes is the size limit of one evidence file.
const MaxEvidenceBytes = 10 << 20

var evidenceTypes = []string{"application/pdf", "image/jpeg", "image/png"}

type evidenceDoc struct {
	Notes   string   `json:"notes,omitempty"`
	BlobIDs []string `json:"blob_ids,omitempty"`
}

// SubmitEvidence sends evidence to the provider where the connector supports
// it, stores the files as blobs (PDF/JPG/PNG, at most 10 MiB each) and the
// notes on the dispute, and re-reads the provider's state.
func (s *Service) SubmitEvidence(ctx context.Context, actor audit.Actor, sc core.OrgScope, id string, in EvidenceInput) (store.Dispute, error) {
	d, err := s.Get(ctx, sc, id)
	if err != nil {
		return store.Dispute{}, err
	}
	if err := s.open(d); err != nil {
		return store.Dispute{}, err
	}
	if in.Text == "" && len(in.Files) == 0 {
		return store.Dispute{}, invalid("evidence needs notes or at least one file")
	}
	ev := connector.Evidence{Text: in.Text}
	for _, f := range in.Files {
		switch {
		case f.Type == "":
			return store.Dispute{}, invalid("each evidence file needs a type")
		case !slices.Contains(evidenceTypes, f.ContentType):
			return store.Dispute{}, invalid("evidence files must be PDF, JPEG or PNG, not %q", f.ContentType)
		case len(f.Data) == 0:
			return store.Dispute{}, invalid("evidence file %q is empty", f.Type)
		case len(f.Data) > MaxEvidenceBytes:
			return store.Dispute{}, invalid("evidence file %q is over 10 MiB", f.Type)
		}
		if ev.Files == nil {
			ev.Files = map[string][]byte{}
		}
		key := f.Type
		for n := 2; ; n++ {
			if _, taken := ev.Files[key]; !taken {
				break
			}
			key = fmt.Sprintf("%s_%d", f.Type, n)
		}
		ev.Files[key] = f.Data
	}
	r, err := s.responder(ctx, d)
	if err != nil {
		return store.Dispute{}, err
	}
	if err := r.SubmitEvidence(ctx, connector.DisputeRef{ProviderDisputeID: d.ProviderDisputeID}, ev); err != nil {
		return store.Dispute{}, fmt.Errorf("disputes: provider refused the evidence: %w", err)
	}
	err = s.o.DB.Write(ctx, func(tx store.Tx) error {
		cur, err := store.GetDispute(ctx, tx, d.ID)
		if err != nil {
			return err
		}
		var doc evidenceDoc
		_ = json.Unmarshal([]byte(cur.Evidence), &doc)
		if in.Text != "" {
			doc.Notes = in.Text
		}
		for _, f := range in.Files {
			bid := ids.New("blob")
			if err := store.InsertBlob(ctx, tx, store.Blob{ID: bid, OrgID: cur.OrgID, Kind: "dispute_evidence", ContentType: f.ContentType,
				SHA256: sha256Hex(f.Data), Data: f.Data, CreatedAt: s.now()}); err != nil {
				return err
			}
			doc.BlobIDs = append(doc.BlobIDs, bid)
		}
		raw, _ := json.Marshal(doc)
		cur.Evidence = string(raw)
		if err := store.UpdateDispute(ctx, tx, &cur, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "dispute.submit_evidence", Target: d.ID,
			After: map[string]any{"files": len(in.Files), "notes": in.Text != ""}})
	})
	if err != nil {
		return store.Dispute{}, err
	}
	cur, err := s.Get(ctx, sc, id)
	if err != nil {
		return store.Dispute{}, err
	}
	return s.refresh(ctx, cur)
}

// IssueCreditNote issues the credit note of a lost or accepted dispute. It is
// idempotent per dispute and audited on every request.
func (s *Service) IssueCreditNote(ctx context.Context, actor audit.Actor, sc core.OrgScope, id string) (*store.CreditNote, error) {
	if s.o.Credit == nil {
		return nil, invalid("invoicing is not enabled")
	}
	var cn *store.CreditNote
	err := s.o.DB.Write(ctx, func(tx store.Tx) error {
		d, err := s.load(ctx, tx, sc, id)
		if err != nil {
			return err
		}
		if st := state.DisputeStatus(d.Status); st != state.DisputeLost && st != state.DisputeAccepted {
			return core.NewError(core.ErrInvalidState, "a credit note is issued for lost or accepted disputes, not %s ones", d.Status)
		}
		p, err := store.GetPaymentForUpdate(ctx, tx, d.PaymentID)
		if err != nil {
			return err
		}
		a, err := store.GetAttempt(ctx, tx, d.AttemptID)
		if err != nil {
			return err
		}
		cn, err = s.o.Credit.IssueDisputeCreditNote(ctx, tx, d, p, a)
		if s.isCutoff(err) {
			return core.NewError(core.ErrInvalidState, "the credit note deadline for the invoice has passed")
		}
		if err != nil {
			return err
		}
		if cn == nil {
			return invalid("the payment has no invoice to credit")
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "dispute.credit_note", Target: id,
			After: map[string]string{"credit_note_id": cn.ID}})
	})
	return cn, err
}
