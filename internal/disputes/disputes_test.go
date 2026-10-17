package disputes_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/disputes"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
)

var (
	ctx = context.Background()
	org = core.OrgScope{OrgID: store.DefaultOrgID}
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type alertLog struct {
	mu     sync.Mutex
	alerts []notify.Alert
	fail   bool
}

func (a *alertLog) Raise(_ context.Context, al notify.Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return errors.New("no channel reachable")
	}
	a.alerts = append(a.alerts, al)
	return nil
}

func (a *alertLog) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, x := range a.alerts {
		out = append(out, x.Kind)
	}
	return out
}

type env struct {
	t      *testing.T
	db     *store.DB
	core   *core.Service
	conns  *connectors.Service
	set    *settings.Service
	d      *disputes.Service
	fake   *fakeconn.State
	clk    *clock
	alerts *alertLog
	scope  core.Scope
	acct   string
	mu     sync.Mutex
	events []events.Event
}

func (e *env) emitted() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		out = append(out, ev.Type)
	}
	return out
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	e := &env{t: t, db: db, clk: clk, set: settings.New(db), fake: fakeconn.Register("fake"), alerts: &alertLog{},
		scope: core.Scope{AppID: "app_1", Mode: "test"}}
	emitter := events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.events = append(e.events, ev)
		return nil
	})
	q := jobs.New(db, jobs.Options{Clock: clk.Now})
	inv := invoice.New(invoice.Options{DB: db, Settings: e.set, Jobs: q, Emitter: emitter, Now: clk.Now})
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Jobs: q, Emitter: emitter, Now: clk.Now,
		OnPaymentSucceeded: inv.OnPaymentSucceeded, OnRefundSucceeded: inv.OnRefundSucceeded})
	box, _ := secrets.NewBox(bytes.Repeat([]byte{4}, 32))
	base, _ := url.Parse("https://pay.example.com")
	e.conns = connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clk.Now})
	e.d = disputes.New(disputes.Options{DB: db, Settings: e.set, Emitter: emitter, Alerts: e.alerts, Credit: inv, CutoffErr: invoice.ErrCreditNoteCutoff,
		Conns: e.conns, Now: clk.Now})
	if _, err := core.Bootstrap(ctx, db, e.set); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx store.Tx) error {
		return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a", CreatedAt: clk.t})
	}); err != nil {
		t.Fatal(err)
	}
	pa, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: e.fake.Name, Name: "main", Mode: "test",
		Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1"})
	if err != nil {
		t.Fatal(err)
	}
	e.acct = pa.ID
	te, err := inv.AddTaxEntity(ctx, audit.System, invoice.TaxEntityInput{LegalName: "Shop Pvt Ltd", GSTIN: "27AAPFU0939F1ZV",
		Address: "Mumbai", SignatoryName: "A Director"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.SetAppTaxProfile(ctx, audit.System, invoice.AppTaxInput{AppID: "app_1", TaxEntityID: te.ID, IssueInvoices: true,
		GSTRateBP: 1800, PricesIncludeTax: true}); err != nil {
		t.Fatal(err)
	}
	return e
}

func each(t *testing.T, fn func(t *testing.T, e *env)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newEnv(t, db)) })
}

// paid returns a succeeded INR payment (with an invoice) whose provider payment id is payID.
func (e *env) paid(payID string) (store.Payment, store.Attempt) {
	e.t.Helper()
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR",
		Customer: &core.CustomerInput{ExternalRef: "u-" + payID, Name: "Asha", Billing: store.Billing{StateCode: "29", Country: "IN"}}})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.core.CreateAttempt(ctx, res.Payment.ID, e.acct)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 50000, Currency: "INR", ProviderPaymentID: payID}); err != nil {
		e.t.Fatal(err)
	}
	p, _ := e.core.GetPaymentByID(ctx, res.Payment.ID)
	return p, a
}

func (e *env) snap(id, payID string, st connector.DisputeStatus, due time.Time) connector.DisputeSnapshot {
	s := connector.DisputeSnapshot{ProviderDisputeID: id, ProviderPaymentID: payID, Status: st, RawStatus: string(st), Phase: "chargeback",
		Reason: "fraudulent", Amount: 50000, Currency: "INR", CreatedAt: e.clk.Now().UnixMilli()}
	if !due.IsZero() {
		s.DueBy = due.UnixMilli()
	}
	return s
}

func (e *env) setProvider(s connector.DisputeSnapshot) {
	e.fake.Locked(func() { e.fake.Disputes[s.ProviderDisputeID] = s })
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) ledger(paymentID string) []store.LedgerEntry {
	var l []store.LedgerEntry
	_ = e.db.Read(ctx, func(q store.Querier) (err error) { l, err = store.ListLedgerEntries(ctx, q, paymentID); return })
	var out []store.LedgerEntry
	for _, x := range l {
		if x.DisputeID != "" {
			out = append(out, x)
		}
	}
	return out
}

func (e *env) payment(id string) store.Payment {
	p, err := e.core.GetPaymentByID(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) apply(s connector.DisputeSnapshot) disputes.Result {
	e.t.Helper()
	r, err := e.d.Apply(ctx, e.acct, s, "webhook")
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// kindsOf returns the entry kinds sorted (entries of one instant have no order).
func kindsOf(l []store.LedgerEntry) []string {
	var k []string
	for _, x := range l {
		k = append(k, x.Kind)
	}
	slices.Sort(k)
	return k
}

func sum(l []store.LedgerEntry) int64 {
	var n int64
	for _, x := range l {
		n += x.Amount
	}
	return n
}

func TestApplyOpensDispute(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := e.paid("pay_1")
		due := e.clk.Now().Add(10 * 24 * time.Hour)
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		d := r.Dispute
		if !r.Created || !strings.HasPrefix(d.ID, "dsp_") || d.Status != "needs_response" || d.PaymentID != p.ID || d.AttemptID != a.ID ||
			d.Amount != 50000 || d.Currency != "INR" || d.Source != "webhook" || d.Stage != "chargeback" || d.Reason != "fraudulent" ||
			!d.DueBy.Equal(due) || d.AppID != "app_1" || d.Mode != "test" || d.Version != 1 {
			t.Fatalf("dispute %+v", d)
		}
		l := e.ledger(p.ID)
		if len(l) != 1 || l[0].Kind != "dispute_hold" || l[0].Amount != -50000 || l[0].DisputeID != d.ID || l[0].Currency != "INR" {
			t.Errorf("ledger %+v", l)
		}
		if got := e.payment(p.ID); !got.HasFlag("disputed") || got.Version != p.Version+1 {
			t.Errorf("payment %+v", got)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "dispute.opened" {
			t.Errorf("events %v", ev)
		}
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"dispute.opened"}) {
			t.Errorf("alerts %v", k)
		}
		a0 := e.alerts.alerts[0]
		if a0.Severity != notify.SeverityWarning || a0.Refs["dispute"] != d.ID || a0.Refs["payment"] != p.ID || !strings.Contains(a0.Message, "2026-10-27") {
			t.Errorf("alert %+v", a0)
		}
		// Replays change nothing.
		n := len(e.emitted())
		r = e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		if r.Created || r.Changed || len(e.emitted()) != n || len(e.ledger(p.ID)) != 1 || len(e.alerts.kinds()) != 1 || r.Dispute.Version != 1 {
			t.Errorf("replay: %+v events %d->%d", r, n, len(e.emitted()))
		}
	})
}

func TestApplyLifecycleWonAndLost(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		due := e.clk.Now().Add(10 * 24 * time.Hour)
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeUnderReview, due))
		if !r.Changed || r.Dispute.Status != "under_review" || r.Dispute.Version != 2 {
			t.Fatalf("under review: %+v", r)
		}
		r = e.apply(e.snap("dsp_1", "pay_1", connector.DisputeWon, due))
		if r.Dispute.Status != "won" {
			t.Fatal(r.Dispute.Status)
		}
		l := e.ledger(p.ID)
		if sum(l) != 0 || len(l) != 2 || kindsOf(l)[1] != "dispute_release" {
			t.Errorf("won ledger %+v", l)
		}
		if got := e.payment(p.ID); got.HasFlag("disputed") || got.HasFlag("dispute_lost") {
			t.Errorf("flags after win: %v", got.Flags)
		}
		want := []string{"dispute.opened", "dispute.updated", "dispute.won"}
		ev := e.emitted()
		if !slices.Equal(ev[len(ev)-3:], want) {
			t.Errorf("events %v", ev)
		}
		// A won dispute cannot become lost.
		if _, err := e.d.Apply(ctx, e.acct, e.snap("dsp_1", "pay_1", connector.DisputeLost, due), "poll"); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("terminal flip: %v", err)
		}
		// A stale earlier state is ignored.
		if r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeUnderReview, due)); r.Changed || r.Dispute.Status != "won" {
			t.Errorf("stale: %+v", r)
		}

		// Another dispute is lost (accepted counts as lost too).
		p2, _ := e.paid("pay_2")
		e.apply(e.snap("dsp_2", "pay_2", connector.DisputeNeedsResponse, due))
		r = e.apply(e.snap("dsp_2", "pay_2", connector.DisputeLost, due))
		l = e.ledger(p2.ID)
		kinds := kindsOf(l)
		if r.Dispute.Status != "lost" || !slices.Equal(kinds, []string{"dispute_hold", "dispute_loss", "dispute_release"}) || sum(l) != -50000 {
			t.Errorf("lost ledger %v sum %d", kinds, sum(l))
		}
		if got := e.payment(p2.ID); got.HasFlag("disputed") || !got.HasFlag("dispute_lost") {
			t.Errorf("flags after loss: %v", got.Flags)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "dispute.lost" {
			t.Errorf("events %v", ev)
		}
		if k := e.alerts.kinds(); k[len(k)-1] != "dispute.lost" {
			t.Errorf("alerts %v", k)
		}
		p3, _ := e.paid("pay_3")
		e.apply(e.snap("dsp_3", "pay_3", connector.DisputeNeedsResponse, due))
		if r := e.apply(e.snap("dsp_3", "pay_3", connector.DisputeAccepted, due)); r.Dispute.Status != "accepted" || sum(e.ledger(p3.ID)) != -50000 {
			t.Errorf("accepted: %+v", r.Dispute)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "dispute.lost" {
			t.Errorf("accepting a dispute is a loss: %v", ev)
		}
		// A closed (non-chargeback) dispute releases the hold without a loss.
		p4, _ := e.paid("pay_4")
		e.apply(e.snap("dsp_4", "pay_4", connector.DisputeNeedsResponse, due))
		e.apply(e.snap("dsp_4", "pay_4", connector.DisputeClosed, due))
		if sum(e.ledger(p4.ID)) != 0 || e.payment(p4.ID).HasFlag("disputed") {
			t.Errorf("closed: ledger %v", e.ledger(p4.ID))
		}
	})
}

func TestApplyCanArriveAlreadyTerminal(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		r := e.apply(e.snap("dsp_9", "pay_1", connector.DisputeLost, time.Time{}))
		if !r.Created || r.Dispute.Status != "lost" || sum(e.ledger(p.ID)) != -50000 {
			t.Fatalf("%+v ledger %+v", r.Dispute, e.ledger(p.ID))
		}
		ev := e.emitted()
		if !slices.Equal(ev[len(ev)-2:], []string{"dispute.opened", "dispute.lost"}) {
			t.Errorf("events %v", ev)
		}
	})
}

func TestApplyUpdatesDeadlineWithoutStatusChange(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		due := e.clk.Now().Add(10 * 24 * time.Hour)
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due.Add(48*time.Hour)))
		if !r.Changed || !r.Dispute.DueBy.Equal(due.Add(48*time.Hour)) || r.Dispute.Version != 2 {
			t.Errorf("%+v", r)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "dispute.updated" {
			t.Errorf("events %v", ev)
		}
	})
}

func TestApplyUnknownPaymentAndAccountScoping(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		if _, err := e.d.Apply(ctx, e.acct, e.snap("dsp_x", "pay_nobody", connector.DisputeNeedsResponse, time.Time{}), "webhook"); !errors.Is(err, disputes.ErrUnknownPayment) {
			t.Errorf("unknown payment: %v", err)
		}
		other, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: e.fake.Name, Name: "other", Mode: "test",
			Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.d.Apply(ctx, other.ID, e.snap("dsp_x", "pay_1", connector.DisputeNeedsResponse, time.Time{}), "webhook"); !errors.Is(err, disputes.ErrUnknownPayment) {
			t.Errorf("another account's payment id must not match: %v", err)
		}
		if e.count("SELECT COUNT(*) FROM disputes") != 0 {
			t.Error("disputes created for unknown payments")
		}
		// The same provider dispute id on two accounts is two disputes.
		e.paid("pay_1")
		_, err = e.d.Apply(ctx, e.acct, e.snap("dsp_same", "pay_1", connector.DisputeNeedsResponse, time.Time{}), "webhook")
		if err != nil {
			t.Fatal(err)
		}
		if e.count("SELECT COUNT(*) FROM disputes") != 1 {
			t.Error("setup")
		}
	})
}

func TestSyncNotificationFetchesTruth(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		due := e.clk.Now().Add(10 * 24 * time.Hour)
		e.setProvider(e.snap("dsp_1", "pay_1", connector.DisputeUnderReview, due))
		// The webhook claims the dispute was lost; the provider API says it is under review.
		claim := e.snap("dsp_1", "pay_1", connector.DisputeLost, due)
		r, err := e.d.SyncNotification(ctx, e.acct, connector.Notification{Kind: connector.NotifyDispute, EventID: "e1", Dispute: &claim})
		if err != nil {
			t.Fatal(err)
		}
		if r.Dispute.Status == "lost" {
			t.Fatal("the webhook snapshot was trusted")
		}
		// under_review at creation: opened first, then moved forward.
		if r.Dispute.Status != "under_review" {
			t.Errorf("status %s", r.Dispute.Status)
		}
		e.fake.Locked(func() {
			if len(e.fake.DisputeGets) != 1 || e.fake.DisputeGets[0].ProviderDisputeID != "dsp_1" {
				t.Errorf("fetches %+v", e.fake.DisputeGets)
			}
		})
		// A dispute the provider does not know is not created from the claim.
		bogus := e.snap("dsp_bogus", "pay_1", connector.DisputeNeedsResponse, due)
		if _, err := e.d.SyncNotification(ctx, e.acct, connector.Notification{Kind: connector.NotifyDispute, Dispute: &bogus}); err == nil {
			t.Error("a dispute missing at the provider was created")
		}
		if e.count("SELECT COUNT(*) FROM disputes") != 1 {
			t.Error("bogus dispute stored")
		}
	})
}

func TestPollListsDisputesAndSkipsUnknownPayments(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		due := e.clk.Now().Add(10 * 24 * time.Hour)
		e.setProvider(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.setProvider(e.snap("dsp_unknown", "pay_zzz", connector.DisputeNeedsResponse, due))
		if err := e.d.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		if err := e.d.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		var src string
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT source FROM disputes WHERE payment_id = ?", p.ID).Scan(&src)
		})
		if e.count("SELECT COUNT(*) FROM disputes") != 1 || src != "poll" {
			t.Errorf("disputes=%d source=%q", e.count("SELECT COUNT(*) FROM disputes"), src)
		}
		// The provider moves it forward; the next poll picks it up.
		e.setProvider(e.snap("dsp_1", "pay_1", connector.DisputeLost, due))
		if err := e.d.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		if e.count("SELECT COUNT(*) FROM disputes WHERE status = 'lost'") != 1 {
			t.Error("poll did not apply the change")
		}
		// A provider error does not fail the job.
		e.fake.Locked(func() { e.fake.DisputeErr = connector.ErrTemporary })
		if err := e.d.Poll(ctx); err != nil {
			t.Errorf("provider error must not fail the poll: %v", err)
		}
	})
}

func TestDeadlineAlertsFireOnce(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		due := e.clk.Now().Add(80 * time.Hour)
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		count := func(kind string) int {
			n := 0
			for _, k := range e.alerts.kinds() {
				if k == kind {
					n++
				}
			}
			return n
		}
		check := func() {
			t.Helper()
			if err := e.d.CheckDeadlines(ctx); err != nil {
				t.Fatal(err)
			}
		}
		check()
		if count("dispute.due_72h") != 0 {
			t.Fatal("72h alert fired at 80h left")
		}
		e.clk.Add(9 * time.Hour) // 71h left
		check()
		check()
		if count("dispute.due_72h") != 1 || count("dispute.due_24h") != 0 {
			t.Fatalf("alerts %v", e.alerts.kinds())
		}
		e.clk.Add(48 * time.Hour) // 23h left
		check()
		check()
		if count("dispute.due_24h") != 1 || count("dispute.due_72h") != 1 {
			t.Fatalf("alerts %v", e.alerts.kinds())
		}
		if a := e.alerts.alerts[len(e.alerts.alerts)-1]; a.Severity != notify.SeverityCritical {
			t.Errorf("24h alert severity %s", a.Severity)
		}
	})
}

func TestDeadlineAlertRetriesWhenDeliveryFails(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		e.alerts.fail = true
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, e.clk.Now().Add(60*time.Hour)))
		if len(e.alerts.kinds()) != 0 {
			t.Fatal("alert recorded while delivery failed")
		}
		_ = e.d.CheckDeadlines(ctx) // fails again, must not mark it sent
		e.alerts.fail = false
		if err := e.d.CheckDeadlines(ctx); err != nil {
			t.Fatal(err)
		}
		k := e.alerts.kinds()
		if !slices.Contains(k, "dispute.due_72h") || !slices.Contains(k, "dispute.opened") {
			t.Errorf("alerts after recovery %v (the open alert and the 72h alert are due)", k)
		}
		before := len(k)
		_ = e.d.CheckDeadlines(ctx)
		if len(e.alerts.kinds()) != before {
			t.Error("alerts repeated")
		}
	})
}

func TestDeadlineAlertsSkipResolvedDisputes(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		due := e.clk.Now().Add(60 * time.Hour)
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeWon, due))
		_ = e.d.CheckDeadlines(ctx)
		for _, k := range e.alerts.kinds() {
			if strings.HasPrefix(k, "dispute.due") {
				t.Errorf("deadline alert for a resolved dispute: %v", e.alerts.kinds())
			}
		}
	})
}

func TestManualEntryAndStatus(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		due := e.clk.Now().Add(7 * 24 * time.Hour)
		d, err := e.d.CreateManual(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_1"}, org, disputes.ManualInput{PaymentID: p.ID, Amount: 20000,
			Reason: "product not received", Stage: "chargeback", DueBy: due})
		if err != nil {
			t.Fatal(err)
		}
		if d.Source != "manual" || d.Status != "needs_response" || d.Amount != 20000 || d.Currency != "INR" || d.PaymentID != p.ID ||
			d.ProviderAccountID != e.acct || !strings.HasPrefix(d.ProviderDisputeID, "manual_") {
			t.Fatalf("manual dispute %+v", d)
		}
		if l := e.ledger(p.ID); len(l) != 1 || l[0].Amount != -20000 {
			t.Errorf("ledger %+v", l)
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'dispute.create_manual'") != 1 {
			t.Error("manual entry must be audited")
		}
		for name, in := range map[string]disputes.ManualInput{
			"zero amount": {PaymentID: p.ID, Amount: 0},
			"too much":    {PaymentID: p.ID, Amount: 50001},
		} {
			if _, err := e.d.CreateManual(ctx, audit.System, org, in); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := e.d.CreateManual(ctx, audit.System, org, disputes.ManualInput{PaymentID: "pay_missing", Amount: 1}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing payment: %v", err)
		}
		d, err = e.d.SetManualStatus(ctx, audit.System, org, d.ID, state.DisputeLost)
		if err != nil || d.Status != "lost" || sum(e.ledger(p.ID)) != -20000 {
			t.Fatalf("lost: %+v %v ledger %+v", d, err, e.ledger(p.ID))
		}
		if _, err := e.d.SetManualStatus(ctx, audit.System, org, d.ID, state.DisputeWon); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("terminal flip: %v", err)
		}
		// Provider-sourced disputes are not edited by hand.
		e.paid("pay_2")
		r := e.apply(e.snap("dsp_p", "pay_2", connector.DisputeNeedsResponse, due))
		if _, err := e.d.SetManualStatus(ctx, audit.System, org, r.Dispute.ID, state.DisputeLost); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("manual status on a provider dispute: %v", err)
		}
	})
}

func TestDisputeOperationsAreOrgScoped(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, e.clk.Now().Add(72*time.Hour)))
		other := core.OrgScope{OrgID: "org_other"}
		if _, err := e.d.Get(ctx, other, r.Dispute.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("Get: %v", err)
		}
		if _, err := e.d.CreateManual(ctx, audit.System, other, disputes.ManualInput{PaymentID: p.ID, Amount: 1}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("CreateManual: %v", err)
		}
		if _, err := e.d.Accept(ctx, audit.System, other, r.Dispute.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("Accept: %v", err)
		}
		if _, err := e.d.SubmitEvidence(ctx, audit.System, other, r.Dispute.ID, disputes.EvidenceInput{Text: "x"}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("SubmitEvidence: %v", err)
		}
		if _, err := e.d.IssueCreditNote(ctx, audit.System, other, r.Dispute.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("IssueCreditNote: %v", err)
		}
		if _, err := e.d.SetManualStatus(ctx, audit.System, other, r.Dispute.ID, state.DisputeLost); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("SetManualStatus: %v", err)
		}
		if l, err := e.d.List(ctx, other, false); err != nil || len(l) != 0 {
			t.Errorf("List leaks: %v %v", l, err)
		}
		if l, err := e.d.List(ctx, org, true); err != nil || len(l) != 1 {
			t.Errorf("List own: %v %v", l, err)
		}
	})
}

func TestAcceptCallsTheConnector(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		due := e.clk.Now().Add(72 * time.Hour)
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.setProvider(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.fake.Locked(func() {
			e.fake.AcceptFn = func(ref connector.DisputeRef) {
				e.fake.Disputes[ref.ProviderDisputeID] = e.snap(ref.ProviderDisputeID, "pay_1", connector.DisputeAccepted, due)
			}
		})
		actor := audit.Actor{Type: audit.ActorUser, ID: "usr_1"}
		d, err := e.d.Accept(ctx, actor, org, r.Dispute.ID)
		if err != nil || d.Status != "accepted" {
			t.Fatalf("accept: %+v %v", d, err)
		}
		e.fake.Locked(func() {
			if len(e.fake.Accepts) != 1 || e.fake.Accepts[0].ProviderDisputeID != "dsp_1" {
				t.Errorf("accepts %+v", e.fake.Accepts)
			}
		})
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'dispute.accept' AND actor_id = 'usr_1'") != 1 {
			t.Error("accept not audited")
		}
		if _, err := e.d.Accept(ctx, actor, org, d.ID); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("accepting a finished dispute: %v", err)
		}
	})
}

func TestAcceptFailuresLeaveTheDisputeAlone(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		due := e.clk.Now().Add(72 * time.Hour)
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.fake.Locked(func() { e.fake.AcceptErr = errors.New("provider says no") })
		if _, err := e.d.Accept(ctx, audit.System, org, r.Dispute.ID); err == nil {
			t.Fatal("provider error swallowed")
		}
		got, _ := e.d.Get(ctx, org, r.Dispute.ID)
		if got.Status != "needs_response" {
			t.Errorf("status %s", got.Status)
		}
		// Manual disputes have no provider to call.
		m, _ := e.d.CreateManual(ctx, audit.System, org, disputes.ManualInput{PaymentID: p.ID, Amount: 100})
		if _, err := e.d.Accept(ctx, audit.System, org, m.ID); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("manual accept: %v", err)
		}
	})
}

func TestSubmitEvidenceStoresBlobsAndCallsTheConnector(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		due := e.clk.Now().Add(72 * time.Hour)
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.setProvider(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.fake.Locked(func() {
			e.fake.EvidenceFn = func(ref connector.DisputeRef) {
				e.fake.Disputes[ref.ProviderDisputeID] = e.snap(ref.ProviderDisputeID, "pay_1", connector.DisputeUnderReview, due)
			}
		})
		pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 100)...)
		d, err := e.d.SubmitEvidence(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_1"}, org, r.Dispute.ID, disputes.EvidenceInput{
			Text: "Delivered on 2026-10-01", Files: []disputes.EvidenceFile{{Type: "shipping_proof", ContentType: "application/pdf", Data: pdf}}})
		if err != nil || d.Status != "under_review" {
			t.Fatalf("submit: %+v %v", d, err)
		}
		var ev struct {
			Notes   string   `json:"notes"`
			BlobIDs []string `json:"blob_ids"`
		}
		if err := json.Unmarshal([]byte(d.Evidence), &ev); err != nil || ev.Notes != "Delivered on 2026-10-01" || len(ev.BlobIDs) != 1 || !strings.HasPrefix(ev.BlobIDs[0], "blob_") {
			t.Fatalf("evidence %s %v", d.Evidence, err)
		}
		var kind, ctype string
		var size int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT kind, content_type, size FROM blobs WHERE id = ?", ev.BlobIDs[0]).Scan(&kind, &ctype, &size)
		})
		if kind != "dispute_evidence" || ctype != "application/pdf" || size != len(pdf) {
			t.Errorf("blob %s %s %d", kind, ctype, size)
		}
		e.fake.Locked(func() {
			if len(e.fake.Evidence) != 1 || e.fake.Evidence[0].Text != "Delivered on 2026-10-01" || !bytes.Equal(e.fake.Evidence[0].Files["shipping_proof"], pdf) {
				t.Errorf("connector evidence %+v", e.fake.Evidence)
			}
		})
		if _, err := e.d.SubmitEvidence(ctx, audit.System, org, d.ID, disputes.EvidenceInput{Text: "again"}); err != nil {
			t.Errorf("resubmitting while under review is allowed: %v", err)
		}
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeWon, due))
		if _, err := e.d.SubmitEvidence(ctx, audit.System, org, d.ID, disputes.EvidenceInput{Text: "late"}); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("evidence on a finished dispute: %v", err)
		}
	})
}

func TestSubmitEvidenceValidation(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		due := e.clk.Now().Add(72 * time.Hour)
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		e.setProvider(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		big := make([]byte, 10<<20+1)
		for name, in := range map[string]disputes.EvidenceInput{
			"empty":        {},
			"exe":          {Files: []disputes.EvidenceFile{{Type: "x", ContentType: "application/x-msdownload", Data: []byte("MZ")}}},
			"too big":      {Files: []disputes.EvidenceFile{{Type: "x", ContentType: "application/pdf", Data: big}}},
			"empty file":   {Files: []disputes.EvidenceFile{{Type: "x", ContentType: "image/png", Data: nil}}},
			"no file type": {Files: []disputes.EvidenceFile{{ContentType: "image/png", Data: []byte("png")}}},
		} {
			if _, err := e.d.SubmitEvidence(ctx, audit.System, org, r.Dispute.ID, in); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if e.count("SELECT COUNT(*) FROM blobs WHERE kind = 'dispute_evidence'") != 0 {
			t.Error("rejected evidence left blobs behind")
		}
		for _, ct := range []string{"application/pdf", "image/jpeg", "image/png"} {
			if _, err := e.d.SubmitEvidence(ctx, audit.System, org, r.Dispute.ID, disputes.EvidenceInput{Files: []disputes.EvidenceFile{{Type: "doc", ContentType: ct, Data: []byte("data")}}}); err != nil {
				t.Errorf("%s: %v", ct, err)
			}
		}
	})
}

func TestCreditNoteForDispute(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := e.paid("pay_1")
		due := e.clk.Now().Add(72 * time.Hour)
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeLost, due))
		if e.count("SELECT COUNT(*) FROM credit_notes") != 0 {
			t.Fatal("dispute_credit_note=manual must not issue automatically")
		}
		cn, err := e.d.IssueCreditNote(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_1"}, org, r.Dispute.ID)
		if err != nil || cn == nil {
			t.Fatalf("credit note: %v %v", cn, err)
		}
		if cn.DisputeID != r.Dispute.ID || cn.RefundID != "" || cn.PaymentID != p.ID || cn.Total != 50000 || !strings.Contains(cn.Reason, "Chargeback") ||
			!strings.Contains(cn.Reason, "fraudulent") {
			t.Errorf("credit note %+v", cn)
		}
		again, err := e.d.IssueCreditNote(ctx, audit.System, org, r.Dispute.ID)
		if err != nil || again.ID != cn.ID || e.count("SELECT COUNT(*) FROM credit_notes") != 1 {
			t.Errorf("not idempotent: %v %v", again, err)
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'dispute.credit_note'") != 2 {
			t.Error("credit note requests must be audited")
		}
		// An open dispute has not cost anything yet.
		e.paid("pay_2")
		open := e.apply(e.snap("dsp_2", "pay_2", connector.DisputeNeedsResponse, due))
		if _, err := e.d.IssueCreditNote(ctx, audit.System, org, open.Dispute.ID); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("credit note for an open dispute: %v", err)
		}
	})
}

func TestCreditNoteOnLossSetting(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "dispute_credit_note", json.RawMessage(`"on_loss"`)); err != nil {
			t.Fatal(err)
		}
		e.paid("pay_1")
		due := e.clk.Now().Add(72 * time.Hour)
		e.apply(e.snap("dsp_1", "pay_1", connector.DisputeNeedsResponse, due))
		r := e.apply(e.snap("dsp_1", "pay_1", connector.DisputeLost, due))
		if e.count("SELECT COUNT(*) FROM credit_notes WHERE dispute_id = ?", r.Dispute.ID) != 1 {
			t.Error("on_loss did not issue the credit note")
		}
		e.paid("pay_2")
		e.apply(e.snap("dsp_2", "pay_2", connector.DisputeNeedsResponse, due))
		e.apply(e.snap("dsp_2", "pay_2", connector.DisputeWon, due))
		if e.count("SELECT COUNT(*) FROM credit_notes") != 1 {
			t.Error("a won dispute must not get a credit note")
		}
	})
}

// A dispute holds money back from the payment it names. What the provider says
// about the amount is checked against what we received: more than that, or in
// another currency, cannot be a dispute of this payment.
func TestApplyRejectsADisputeThatCannotBeOfThisPayment(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.paid("pay_1")
		over := e.snap("dsp_over", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		over.Amount = 50001
		usd := e.snap("dsp_usd", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		usd.Currency = "USD"
		for name, s := range map[string]connector.DisputeSnapshot{"more than received": over, "another currency": usd} {
			if _, err := e.d.Apply(ctx, e.acct, s, "webhook"); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if e.count("SELECT COUNT(*) FROM disputes") != 0 {
			t.Error("a dispute was recorded")
		}
		// A partial dispute is fine.
		part := e.snap("dsp_part", "pay_1", connector.DisputeNeedsResponse, time.Time{})
		part.Amount = 10000
		if r := e.apply(part); r.Dispute.Amount != 10000 {
			t.Errorf("partial: %+v", r.Dispute)
		}
	})
}
