// Package reconcile is the daily reconciliation of spec section 11: for each
// provider account it lists the provider's payments and refunds for the
// window D-2..D, diffs them with ours (internal/recon), lets the provider win
// on status mismatches, and records and alerts on everything else.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/ids"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/recon"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/money"
)

// Alerter raises ops alerts (*notify.Alerts).
type Alerter interface {
	Raise(ctx context.Context, al notify.Alert) error
}

// JobTick is the job that checks, every few minutes, whether a reconciliation
// is due.
const JobTick = "recon.tick"

// JobAccount is the job that reconciles one provider account right away (the
// dashboard's "Run now").
const JobAccount = "recon.account"

type accountPayload struct {
	AccountID string `json:"account_id"`
}

// AccountJob builds the job that reconciles one provider account now. Asking
// twice while one is pending queues it once.
func AccountJob(accountID string) jobs.Job {
	return jobs.Job{Kind: JobAccount, Payload: accountPayload{AccountID: accountID}, DedupeKey: JobAccount + ":" + accountID, MaxAttempts: 1}
}

// Options configures a Service.
type Options struct {
	DB       *store.DB
	Core     *core.Service
	Conns    *connectors.Service
	Settings *settings.Service
	Alerts   Alerter // nil disables alerts
	// Loc is PAYMUX_TZ: recon_time and the D-2..D window are in this zone.
	Loc    *time.Location
	Logger *slog.Logger
	Now    func() time.Time
	// KnownSubscription says whether Paymux has the provider subscription
	// (of this provider account). A payment of a known subscription that
	// Paymux does not have is a provider-managed renewal that was missed.
	KnownSubscription func(ctx context.Context, accountID, providerSubscriptionID string) bool
	// LinkRenewal records such a renewal payment on its subscription (the
	// subscriptions service creates the renewal payment). Without it, or when
	// it fails, the renewal is stored as renewal_to_link and retried by the
	// next run.
	LinkRenewal func(ctx context.Context, accountID string, pay connector.PaymentSnapshot) error
	// Subscriptions applies the provider's mandate and subscription status
	// to Paymux's (the subscriptions service). Without it mandates and
	// subscriptions are not reconciled.
	Subscriptions SubscriptionSink
}

// Service runs reconciliations.
type Service struct{ o Options }

// New builds a Service.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Loc == nil {
		o.Loc = time.UTC
	}
	return &Service{o: o}
}

func (s *Service) now() time.Time { return s.o.Now().UTC() }

func invalid(format string, a ...any) error { return core.NewError(core.ErrInvalid, format, a...) }

// window is [start of day D-2 in Loc, now].
func (s *Service) window() (from, to time.Time) {
	now := s.now()
	y, m, d := now.In(s.o.Loc).Date()
	return time.Date(y, m, d-2, 0, 0, 0, 0, s.o.Loc).UTC(), now
}

// payment statuses are compared in a small shared vocabulary: an attempt that
// is created or pending is "open".
func openStatus(attemptStatus string) string {
	switch state.AttemptStatus(attemptStatus) {
	case state.AttemptCreated, state.AttemptPending:
		return "open"
	}
	return attemptStatus
}

// Provider payment-attempt history retains the provider's raw status for the
// operator UI. Normalize the common terminal spellings before comparing that
// history with connector snapshots, which use the core attempt vocabulary.
func providerAttemptStatus(status string) string {
	switch status {
	case "captured", "succeeded", "paid", "refunded":
		return string(state.AttemptCaptured)
	case "failed", "canceled", "cancelled", "expired":
		return string(state.AttemptFailed)
	case "authorized", "processing":
		return string(state.AttemptAuthorized)
	case "created", "pending", "requires_action":
		return "open"
	default:
		return openStatus(status)
	}
}

const maxPages = 200

// RunAll reconciles every active provider account. A failing account is
// recorded and alerted and does not stop the others.
func (s *Service) RunAll(ctx context.Context) error {
	var accounts []store.ProviderAccount
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { accounts, err = store.ListActiveProviderAccounts(ctx, q); return }); err != nil {
		return err
	}
	for _, pa := range accounts {
		if _, err := s.RunAccount(ctx, pa.ID); err != nil {
			s.o.Logger.Warn("reconcile: account failed", "account", pa.ID, "err", err)
		}
	}
	return nil
}

// Tick runs the reconciliation of every account that has not run since the
// latest recon_time (in PAYMUX_TZ). An account that never ran waits for the
// first recon_time that passes after it was created. The setting is read on
// every tick, so changes apply without a restart, and a missed slot is made up.
func (s *Service) Tick(ctx context.Context) error {
	org, err := s.orgID(ctx)
	if err != nil {
		return err
	}
	v, err := s.o.Settings.ResolveOrg(ctx, org, "recon_time")
	if err != nil {
		return err
	}
	var hhmm string
	if err := v.Decode(&hhmm); err != nil {
		return err
	}
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil {
		return fmt.Errorf("reconcile: recon_time %q: %w", hhmm, err)
	}
	now := s.now()
	y, mo, d := now.In(s.o.Loc).Date()
	slot := time.Date(y, mo, d, h, m, 0, 0, s.o.Loc)
	if slot.After(now) {
		slot = time.Date(y, mo, d-1, h, m, 0, 0, s.o.Loc)
	}
	today := slot.In(s.o.Loc).Day() == now.In(s.o.Loc).Day() && slot.In(s.o.Loc).Month() == now.In(s.o.Loc).Month()
	var accounts []store.ProviderAccount
	if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { accounts, err = store.ListActiveProviderAccounts(ctx, q); return }); err != nil {
		return err
	}
	for _, pa := range accounts {
		var last time.Time
		var had bool
		if err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { last, had, err = store.LastReconStart(ctx, q, pa.ID); return }); err != nil {
			return err
		}
		due := had && last.Before(slot.UTC()) || !had && today
		if !due {
			continue
		}
		if _, err := s.RunAccount(ctx, pa.ID); err != nil {
			s.o.Logger.Warn("reconcile: account failed", "account", pa.ID, "err", err)
		}
	}
	return nil
}

func (s *Service) orgID(ctx context.Context) (string, error) {
	var org string
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { org, err = store.FirstOrgID(ctx, q); return })
	return org, err
}

// Register installs the tick job and its schedule (every 5 minutes).
func (s *Service) Register(q *jobs.Queue) error {
	q.Register(JobTick, func(ctx context.Context, _ json.RawMessage) error { return s.Tick(ctx) })
	q.Register(JobAccount, func(ctx context.Context, raw json.RawMessage) error {
		var p accountPayload
		if err := json.Unmarshal(raw, &p); err != nil || p.AccountID == "" {
			return jobs.Permanent(fmt.Errorf("%s: bad payload %s", JobAccount, raw))
		}
		run, err := s.RunAccount(ctx, p.AccountID)
		if err != nil && run.Status == "failed" {
			return nil // recorded on the run and alerted; running it again now would not help
		}
		return err
	})
	return q.AddSchedule(jobs.Schedule{Name: JobTick, Kind: JobTick, Every: 5 * time.Minute})
}

// RunAccount reconciles one provider account for the window D-2..D and
// returns the recorded run. A connector that cannot list payments is skipped
// (a run with nothing checked). Provider failures produce a failed run and a
// critical alert.
func (s *Service) RunAccount(ctx context.Context, accountID string) (store.ReconRun, error) {
	var pa store.ProviderAccount
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) { pa, err = store.GetProviderAccount(ctx, q, accountID); return })
	if err != nil {
		return store.ReconRun{}, err
	}
	from, to := s.window()
	run := store.ReconRun{ID: ids.New("rcr"), OrgID: pa.OrgID, ProviderAccountID: pa.ID, WindowFrom: from, WindowTo: to, StartedAt: s.now()}
	if err := s.o.DB.Write(ctx, func(tx store.Tx) error { return store.InsertReconRun(ctx, tx, run) }); err != nil {
		return run, err
	}
	runErr := s.reconcile(ctx, pa, &run)
	run.FinishedAt, run.Status = s.now(), "ok"
	if runErr != nil {
		run.Status, run.Error = "failed", runErr.Error()
	}
	if err := s.o.DB.Write(ctx, func(tx store.Tx) error { return store.FinishReconRun(ctx, tx, run) }); err != nil {
		return run, err
	}
	if runErr != nil {
		s.raise(ctx, notify.Alert{Severity: notify.SeverityCritical, Kind: "recon.failed",
			Message: fmt.Sprintf("Reconciliation of %s account %s failed: %v", pa.Connector, pa.Name, runErr),
			Refs:    map[string]string{"provider_account": pa.ID, "run": run.ID}})
	}
	return run, runErr
}

func (s *Service) raise(ctx context.Context, al notify.Alert) {
	if s.o.Alerts == nil {
		return
	}
	if err := s.o.Alerts.Raise(ctx, al); err != nil {
		s.o.Logger.Warn("reconcile: alert not delivered", "kind", al.Kind, "err", err)
	}
}

func (s *Service) reconcile(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun) error {
	live, err := s.o.Conns.Get(ctx, pa.ID)
	if err != nil {
		return err
	}
	lister, ok := live.Connector.(connector.Lister)
	if !ok {
		return nil // nothing to compare against
	}
	tr := connector.TimeRange{From: run.WindowFrom.UnixMilli(), To: run.WindowTo.UnixMilli()}

	var theirsPay []connector.PaymentSnapshot
	for cur, n := connector.Cursor(""), 0; n < maxPages; n++ {
		pg, err := lister.ListPayments(ctx, tr, cur)
		if err != nil {
			return fmt.Errorf("listing payments: %w", err)
		}
		theirsPay = append(theirsPay, pg.Items...)
		if pg.Next == "" {
			break
		}
		cur = pg.Next
	}
	var theirsRef []connector.RefundSnapshot
	for cur, n := connector.Cursor(""), 0; n < maxPages; n++ {
		pg, err := lister.ListRefunds(ctx, tr, cur)
		if err != nil {
			return fmt.Errorf("listing refunds: %w", err)
		}
		theirsRef = append(theirsRef, pg.Items...)
		if pg.Next == "" {
			break
		}
		cur = pg.Next
	}
	run.Checked = len(theirsPay) + len(theirsRef)

	if err := s.reconcilePayments(ctx, pa, run, theirsPay); err != nil {
		return err
	}
	if err := s.reconcileRefunds(ctx, pa, run, theirsRef); err != nil {
		return err
	}
	if err := s.reconcileSettlements(ctx, pa, run, live.Connector); err != nil {
		return err
	}
	return s.reconcileMandatesAndSubscriptions(ctx, pa, run, live.Connector)
}

// ---- payments ----

func (s *Service) reconcilePayments(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun, theirs []connector.PaymentSnapshot) error {
	// Attempts created a day before the window can still be paid inside it.
	var attempts []store.ReconAttempt
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		attempts, err = store.ListReconAttempts(ctx, q, pa.ID, run.WindowFrom.Add(-24*time.Hour), run.WindowTo)
		return
	})
	if err != nil {
		return err
	}
	byPayID := map[string]connector.PaymentSnapshot{}
	byOrder := map[string]string{} // provider order id -> provider payment id
	byRef := map[string]string{}   // our reference (the provider's transaction id) -> provider payment id
	var theirRecs []recon.Record
	for _, t := range theirs {
		if t.ProviderPaymentID == "" {
			continue
		}
		byPayID[t.ProviderPaymentID] = t
		if t.ProviderOrderID != "" {
			byOrder[t.ProviderOrderID] = t.ProviderPaymentID
		}
		if t.ProviderRef != "" {
			byRef[t.ProviderRef] = t.ProviderPaymentID
		}
		theirRecs = append(theirRecs, recon.Record{ProviderID: t.ProviderPaymentID, Status: string(checkout.ToSnapshot(t).Status),
			Amount: t.Amount, Currency: t.Currency, SubscriptionID: t.SubscriptionID})
	}
	for i := range theirRecs {
		theirRecs[i].Status = openStatus(theirRecs[i].Status)
	}
	var ours []recon.Record
	for _, a := range attempts {
		id := a.ProviderPaymentID
		if id == "" {
			id = byOrder[a.ProviderOrderID] // a payment we never heard about, found by its order
		}
		if id == "" && a.ProviderRef != "" {
			id = byRef[a.ProviderRef] // or by our reference, when the provider keys its transactions on it
		}
		if id == "" {
			continue
		}
		rec := recon.Record{ProviderID: id, Status: providerAttemptStatus(a.Status), Amount: a.PaymentAmount, Currency: a.PaymentCurrency}
		if a.Status == string(state.AttemptCaptured) {
			rec.Amount, rec.Currency = a.AmountReceived, a.CurrencyReceived
		}
		ours = append(ours, rec)
	}
	var known func(string) bool
	if s.o.KnownSubscription != nil {
		known = func(id string) bool { return s.o.KnownSubscription(ctx, pa.ID, id) }
	}
	items := recon.Diff(ours, theirRecs, known)
	for _, it := range items {
		snap := byPayID[it.ProviderID]
		fixed := false
		var note recordNote
		if it.Kind == recon.KindRenewalToLink && s.o.LinkRenewal != nil {
			if err := s.o.LinkRenewal(ctx, pa.ID, snap); err != nil {
				s.o.Logger.Warn("reconcile: linking a renewal failed; it stays to be linked", "account", pa.ID, "payment", it.ProviderID, "err", err)
			} else {
				fixed = true
			}
		}
		if it.AutoFix {
			if err := s.fixPayment(ctx, pa, snap); err != nil {
				s.o.Logger.Warn("reconcile: auto-fix failed", "account", pa.ID, "payment", it.ProviderID, "err", err)
				it.AutoFix = false
				note = failedFix("payment", it.ProviderID, err)
			} else {
				fixed = true
			}
		}
		if err := s.record(ctx, pa, run, it, fixed, note); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) fixPayment(ctx context.Context, pa store.ProviderAccount, snap connector.PaymentSnapshot) error {
	var a store.Attempt
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		a, err = store.FindAttempt(ctx, q, pa.ID, snap.ProviderRef, snap.ProviderOrderID, snap.ProviderPaymentID)
		return
	})
	if err != nil {
		return err
	}
	_, err = s.o.Core.ApplyAttemptSnapshot(ctx, a.ID, checkout.ToSnapshot(snap))
	return err
}

// ---- refunds ----

func refundStatus(s connector.RefundStatus) string { return string(s) }

func (s *Service) reconcileRefunds(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun, theirs []connector.RefundSnapshot) error {
	var ourRefunds []store.ReconRefund
	err := s.o.DB.Read(ctx, func(q store.Querier) (err error) {
		ourRefunds, err = store.ListReconRefunds(ctx, q, pa.ID, run.WindowFrom.Add(-24*time.Hour), run.WindowTo)
		return
	})
	if err != nil {
		return err
	}
	byID := map[string]connector.RefundSnapshot{}
	byRef := map[string]string{} // provider_ref (receipt) -> provider refund id
	var theirRecs []recon.Record
	for _, t := range theirs {
		if t.ProviderRefundID == "" {
			continue
		}
		byID[t.ProviderRefundID] = t
		if t.ProviderRef != "" {
			byRef[t.ProviderRef] = t.ProviderRefundID
		}
		theirRecs = append(theirRecs, recon.Record{ProviderID: t.ProviderRefundID, Status: refundStatus(t.Status), Amount: t.Amount, Currency: t.Currency})
	}
	ourByProviderID := map[string]string{} // provider refund id -> our refund id
	var ours []recon.Record
	for _, r := range ourRefunds {
		id := r.ProviderRefundID
		if id == "" {
			id = byRef[r.ProviderRef]
		}
		if id == "" {
			continue
		}
		ourByProviderID[id] = r.RefundID
		ours = append(ours, recon.Record{ProviderID: id, Status: r.Status, Amount: r.Amount, Currency: r.Currency})
	}
	for _, it := range recon.Diff(ours, theirRecs, nil) {
		fixed := false
		var note recordNote
		if it.Kind == recon.KindUnknownProviderObject {
			// A refund Paymux has no row for. If it belongs to one of our
			// payments the provider made it on its own (its dashboard, a
			// resolution centre) and its webhook was missed: book it, from the
			// provider's record, as initiated_by=provider. Anything else stays
			// an open item for a person.
			imported, err := checkout.ImportRefund(ctx, s.o.DB, s.o.Core, pa.ID, byID[it.ProviderID])
			switch {
			case err != nil:
				s.o.Logger.Warn("reconcile: importing a provider refund failed", "account", pa.ID, "refund", it.ProviderID, "err", err)
				if !errors.Is(err, core.ErrInvalid) && !errors.Is(err, core.ErrNotRefundable) {
					note = failedFix("refund", it.ProviderID, err)
				}
			default:
				fixed = imported
			}
		}
		if it.AutoFix {
			snap := byID[it.ProviderID]
			_, err := s.o.Core.ApplyRefundSnapshot(ctx, ourByProviderID[it.ProviderID], core.RefundSnapshot{
				Status: state.RefundStatus(snap.Status), ProviderRefundID: snap.ProviderRefundID, FailureReason: snap.RawStatus})
			if err != nil {
				s.o.Logger.Warn("reconcile: refund auto-fix failed", "account", pa.ID, "refund", it.ProviderID, "err", err)
				note = failedFix("refund", it.ProviderID, err)
			} else {
				fixed = true
			}
		}
		if err := s.record(ctx, pa, run, it, fixed, note); err != nil {
			return err
		}
	}
	return nil
}

// ---- recording ----

// recordNote customises the alert of a recorded item: a message of its own
// and an alert kind other than "recon.<item kind>".
type recordNote struct{ detail, kind string }

func failedFix(what, id string, err error) recordNote {
	return recordNote{kind: "recon.autofix_failed",
		detail: fmt.Sprintf("Reconciliation could not apply the provider's state of %s %s: %v", what, id, err)}
}

// record stores an item. Auto-fixed items are always stored. Anything else is
// stored and alerted once per object and kind while it stays open.
func (s *Service) record(ctx context.Context, pa store.ProviderAccount, run *store.ReconRun, it recon.Item, fixed bool, note recordNote) error {
	ours := ""
	if it.Ours != nil {
		b, _ := json.Marshal(it.Ours)
		ours = string(b)
	}
	theirs, _ := json.Marshal(it.Theirs)
	item := store.ReconItem{ID: ids.New("rci"), RunID: run.ID, OrgID: pa.OrgID, ProviderAccountID: pa.ID, Kind: it.Kind,
		ProviderID: it.ProviderID, Ours: ours, Theirs: string(theirs), State: "open", CreatedAt: s.now()}
	switch {
	case fixed:
		item.State = "fixed"
	case it.Kind == recon.KindRenewalToLink:
		item.State = "to_link"
	}
	var existing store.ReconItem
	var seen bool
	err := s.o.DB.Write(ctx, func(tx store.Tx) (err error) {
		if fixed {
			// A fixed item supersedes any earlier open item of the same object.
			if err := store.CloseReconItems(ctx, tx, pa.ID, it.Kind, it.ProviderID); err != nil {
				return err
			}
		} else {
			if existing, seen, err = store.FindOpenReconItem(ctx, tx, pa.ID, it.Kind, it.ProviderID); err != nil {
				return err
			}
			if seen {
				return store.TouchReconItem(ctx, tx, existing.ID, run.ID)
			}
		}
		return store.InsertReconItem(ctx, tx, item)
	})
	if err != nil {
		return err
	}
	run.Items++
	if fixed {
		run.Fixed++
		return nil
	}
	if seen || item.State == "to_link" {
		return nil
	}
	kind := "recon." + it.Kind
	if note.kind != "" {
		kind = note.kind
	}
	s.raise(ctx, notify.Alert{Severity: notify.SeverityWarning, Kind: kind,
		Message: describe(it, pa, note.detail), Refs: map[string]string{"provider_account": pa.ID, "provider_id": it.ProviderID, "run": run.ID}})
	return nil
}

func describe(it recon.Item, pa store.ProviderAccount, detail string) string {
	if detail != "" {
		return detail
	}
	switch it.Kind {
	case recon.KindAmountMismatch, recon.KindCurrencyMismatch:
		return fmt.Sprintf("%s account %s: %s differs for %s: ours %s, provider %s", pa.Connector, pa.Name, it.Kind, it.ProviderID,
			formatMoney(it.Ours.Amount, it.Ours.Currency), formatMoney(it.Theirs.Amount, it.Theirs.Currency))
	case recon.KindUnknownProviderObject:
		return fmt.Sprintf("%s account %s has %s %s (%s, %s) that Paymux does not know", pa.Connector, pa.Name, "an object", it.ProviderID,
			formatMoney(it.Theirs.Amount, it.Theirs.Currency), it.Theirs.Status)
	}
	return fmt.Sprintf("%s account %s: %s for %s", pa.Connector, pa.Name, it.Kind, it.ProviderID)
}

func formatMoney(amount int64, currency string) string {
	decimal, err := money.FormatDecimal(amount, currency)
	if err != nil {
		return fmt.Sprintf("%d minor units %s", amount, currency)
	}
	return decimal + " " + currency
}

// Resolve closes an open item with a note, audited.
func (s *Service) Resolve(ctx context.Context, actor audit.Actor, sc core.OrgScope, itemID, note string) error {
	if note == "" {
		return invalid("a note is required to resolve a reconciliation item")
	}
	return s.o.DB.Write(ctx, func(tx store.Tx) error {
		it, err := store.GetReconItem(ctx, tx, itemID)
		if errors.Is(err, store.ErrNotFound) || err == nil && (sc.OrgID == "" || it.OrgID != sc.OrgID) {
			return core.NewError(core.ErrNotFound, "reconciliation item not found")
		}
		if err != nil {
			return err
		}
		if err := store.ResolveReconItem(ctx, tx, itemID, note, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{OrgID: sc.OrgID, Actor: actor, Action: "recon.resolve", Target: itemID,
			Before: map[string]string{"state": it.State, "kind": it.Kind}, After: map[string]string{"state": "resolved", "note": note}})
	})
}
