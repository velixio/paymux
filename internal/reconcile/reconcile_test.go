package reconcile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/fakeconn"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/reconcile"
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
}

func (a *alertLog) Raise(_ context.Context, al notify.Alert) error {
	a.mu.Lock()
	defer a.mu.Unlock()
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
	rec    *reconcile.Service
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
	e := &env{t: t, db: db, clk: clk, set: settings.New(db), fake: fakeconn.Register("fake"), alerts: &alertLog{}, scope: core.Scope{AppID: "app_1", Mode: "test"}}
	emitter := events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.events = append(e.events, ev)
		return nil
	})
	q := jobs.New(db, jobs.Options{Clock: clk.Now})
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Jobs: q, Emitter: emitter, Now: clk.Now})
	box, _ := secrets.NewBox(bytes.Repeat([]byte{4}, 32))
	base, _ := url.Parse("https://pay.example.com")
	e.conns = connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clk.Now})
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
	e.rec = reconcile.New(reconcile.Options{DB: db, Core: e.core, Conns: e.conns, Settings: e.set, Alerts: e.alerts, Loc: time.UTC, Now: clk.Now,
		KnownSubscription: func(id string) bool { return id == "sub_known" },
		SubscriptionOf: func(s connector.PaymentSnapshot) string {
			if s.ProviderRef == "RENEWAL" {
				return "sub_known"
			}
			return ""
		}})
	return e
}

func each(t *testing.T, fn func(t *testing.T, e *env)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newEnv(t, db)) })
}

// attempt creates a payment and an attempt that reached the provider with order id order.
func (e *env) attempt(order string) (store.Payment, store.Attempt) {
	e.t.Helper()
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 50000, Currency: "INR"})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.core.CreateAttempt(ctx, res.Payment.ID, e.acct)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.core.RecordCheckout(ctx, a.ID, order); err != nil {
		e.t.Fatal(err)
	}
	return res.Payment, a
}

func (e *env) provider(ref string, s connector.PaymentSnapshot) {
	if s.CreatedAt == 0 {
		s.CreatedAt = e.clk.Now().Add(-time.Hour).UnixMilli()
	}
	e.fake.Locked(func() { e.fake.Payments[ref] = s })
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) items(runID string) map[string]store.ReconItem {
	var l []store.ReconItem
	_ = e.db.Read(ctx, func(q store.Querier) (err error) { l, err = store.ListReconItemsOfRun(ctx, q, runID); return })
	out := map[string]store.ReconItem{}
	for _, i := range l {
		out[i.Kind+":"+i.ProviderID] = i
	}
	return out
}

func paid(order, payID string, amount int64, cur string) connector.PaymentSnapshot {
	return connector.PaymentSnapshot{Status: connector.PaymentSucceeded, RawStatus: "captured", Amount: amount, Currency: cur,
		ProviderPaymentID: payID, ProviderOrderID: order, Method: "upi"}
}

func TestStatusMismatchIsAutoFixed(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := e.attempt("order_1")
		e.provider("ref1", paid("order_1", "pay_1", 50000, "INR")) // webhooks were missed; matched by order id
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "ok" || run.Checked != 1 || run.Items != 1 || run.Fixed != 1 || run.FinishedAt.IsZero() {
			t.Fatalf("run %+v", run)
		}
		it := e.items(run.ID)["status_mismatch:pay_1"]
		if it.State != "fixed" || it.Ours == "" || it.Theirs == "" {
			t.Fatalf("items %+v", e.items(run.ID))
		}
		got, _ := e.core.GetAttempt(ctx, a.ID)
		pay, _ := e.core.GetPaymentByID(ctx, p.ID)
		if got.Status != "captured" || got.ProviderPaymentID != "pay_1" || pay.Status != "succeeded" || pay.AmountReceived != 50000 {
			t.Errorf("attempt %+v payment %+v", got, pay)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "payment.succeeded" {
			t.Errorf("events %v", ev)
		}
		if len(e.alerts.kinds()) != 0 {
			t.Errorf("an auto-fix must not alert: %v", e.alerts.kinds())
		}
		// Running again finds nothing to do.
		run2, _ := e.rec.RunAccount(ctx, e.acct)
		if run2.Items != 0 || run2.Fixed != 0 {
			t.Errorf("second run %+v", run2)
		}
	})
}

func TestAmountAndCurrencyMismatchesAreFlaggedOnceAndAlerted(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, a1 := e.attempt("order_1")
		_, a2 := e.attempt("order_2")
		for _, a := range []store.Attempt{a1, a2} {
			if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 50000, Currency: "INR",
				ProviderPaymentID: "pay_" + a.ID[len(a.ID)-4:]}); err != nil {
				t.Fatal(err)
			}
		}
		id1, id2 := "pay_"+a1.ID[len(a1.ID)-4:], "pay_"+a2.ID[len(a2.ID)-4:]
		e.provider("r1", paid("order_1", id1, 49999, "INR"))
		e.provider("r2", paid("order_2", id2, 50000, "USD"))
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		items := e.items(run.ID)
		if items["amount_mismatch:"+id1].State != "open" || items["currency_mismatch:"+id2].State != "open" || run.Items != 2 || run.Fixed != 0 {
			t.Fatalf("run %+v items %+v", run, items)
		}
		k := e.alerts.kinds()
		slices.Sort(k)
		if !slices.Equal(k, []string{"recon.amount_mismatch", "recon.currency_mismatch"}) {
			t.Errorf("alerts %v", k)
		}
		a := e.alerts.alerts[0]
		if a.Severity != notify.SeverityWarning || a.Refs["provider_account"] != e.acct {
			t.Errorf("alert %+v", a)
		}
		// Nothing was changed on our side.
		if got, _ := e.core.GetAttempt(ctx, a1.ID); got.AmountReceived != 50000 {
			t.Errorf("flag-only item modified the attempt: %+v", got)
		}
		// The same discrepancy found again neither duplicates the item nor alerts again.
		run2, _ := e.rec.RunAccount(ctx, e.acct)
		if e.count("SELECT COUNT(*) FROM recon_items WHERE kind IN ('amount_mismatch','currency_mismatch')") != 2 || len(e.alerts.kinds()) != 2 {
			t.Errorf("duplicate items or alerts: %d items, alerts %v", e.count("SELECT COUNT(*) FROM recon_items"), e.alerts.kinds())
		}
		if len(e.items(run2.ID)) != 2 {
			t.Errorf("the open items should point at the latest run: %v", e.items(run2.ID))
		}
	})
}

func TestUnknownProviderObjectAndRenewalToLink(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.provider("stranger", paid("order_x", "pay_stranger", 100, "INR"))
		e.provider("RENEWAL", paid("", "pay_renew", 100, "INR"))
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		items := e.items(run.ID)
		if items["unknown_provider_object:pay_stranger"].State != "open" || items["renewal_to_link:pay_renew"].State != "to_link" || run.Items != 2 {
			t.Fatalf("items %+v", items)
		}
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"recon.unknown_provider_object"}) {
			t.Errorf("only the unknown payment alerts; a renewal is stored to be linked: %v", k)
		}
	})
}

func TestRefundsAreReconciledToo(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, a := e.attempt("order_1")
		if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 50000, Currency: "INR", ProviderPaymentID: "pay_1"}); err != nil {
			t.Fatal(err)
		}
		e.provider("r1", paid("order_1", "pay_1", 50000, "INR"))
		r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 10000})
		if err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			e.fake.Refunds[r.ProviderRef] = connector.RefundSnapshot{ProviderRef: r.ProviderRef, ProviderRefundID: "rfnd_1", ProviderPaymentID: "pay_1",
				Status: connector.RefundSucceeded, Amount: 10000, Currency: "INR", CreatedAt: e.clk.Now().UnixMilli()}
			e.fake.Refunds["stray"] = connector.RefundSnapshot{ProviderRefundID: "rfnd_stray", ProviderPaymentID: "pay_1",
				Status: connector.RefundSucceeded, Amount: 500, Currency: "INR", CreatedAt: e.clk.Now().UnixMilli()}
		})
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		items := e.items(run.ID)
		if items["status_mismatch:rfnd_1"].State != "fixed" || items["unknown_provider_object:rfnd_stray"].State != "open" {
			t.Fatalf("items %+v", items)
		}
		got, _ := e.core.GetRefund(ctx, e.scope, r.ID)
		pay, _ := e.core.GetPaymentByID(ctx, p.ID)
		if got.Status != "succeeded" || got.ProviderRefundID != "rfnd_1" || pay.AmountRefunded != 10000 {
			t.Errorf("refund %+v payment %+v", got, pay)
		}
		if !slices.Contains(e.alerts.kinds(), "recon.unknown_provider_object") {
			t.Errorf("alerts %v", e.alerts.kinds())
		}
	})
}

func TestWindowIsTwoDaysBackInTheConfiguredZone(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		ist := time.FixedZone("IST", 5*3600+1800)
		rec := reconcile.New(reconcile.Options{DB: e.db, Core: e.core, Conns: e.conns, Settings: e.set, Alerts: e.alerts, Loc: ist, Now: e.clk.Now})
		e.provider("old", func() connector.PaymentSnapshot {
			s := paid("o", "pay_old", 1, "INR")
			s.CreatedAt = e.clk.Now().Add(-4 * 24 * time.Hour).UnixMilli()
			return s
		}())
		if _, err := rec.RunAccount(ctx, e.acct); err != nil {
			t.Fatal(err)
		}
		var tr connector.TimeRange
		e.fake.Locked(func() { tr = e.fake.ListRanges[0] })
		// Now is 2026-10-17 17:30 IST: the window opens at 2026-10-15 00:00 IST.
		wantFrom := time.Date(2026, 10, 15, 0, 0, 0, 0, ist).UnixMilli()
		if tr.From != wantFrom || tr.To != e.clk.Now().UnixMilli() {
			t.Errorf("window %v..%v, want %v..%v", time.UnixMilli(tr.From), time.UnixMilli(tr.To), time.UnixMilli(wantFrom), e.clk.Now())
		}
		if e.count("SELECT COUNT(*) FROM recon_items") != 0 {
			t.Error("a payment outside the window was reconciled")
		}
	})
}

func TestFailedRunsAreRecordedAndAlerted(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.fake.Locked(func() { e.fake.ListErr = connector.ErrTemporary })
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err == nil || run.Status != "failed" || run.Error == "" {
			t.Fatalf("run %+v err %v", run, err)
		}
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"recon.failed"}) || e.alerts.alerts[0].Severity != notify.SeverityCritical {
			t.Errorf("alerts %v", k)
		}
		// RunAll keeps going and reports nothing fatal.
		if err := e.rec.RunAll(ctx); err != nil {
			t.Errorf("RunAll: %v", err)
		}
	})
}

func TestTickRunsOncePerDayAfterReconTime(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		runs := func() int { return e.count("SELECT COUNT(*) FROM recon_runs") }
		e.clk.t = time.Date(2026, 10, 17, 2, 0, 0, 0, time.UTC) // before 03:00
		if err := e.rec.Tick(ctx); err != nil || runs() != 0 {
			t.Fatalf("before recon_time: %v runs=%d", err, runs())
		}
		e.clk.Add(70 * time.Minute) // 03:10
		if err := e.rec.Tick(ctx); err != nil || runs() != 1 {
			t.Fatalf("after recon_time: %v runs=%d", err, runs())
		}
		e.clk.Add(5 * time.Minute)
		_ = e.rec.Tick(ctx)
		if runs() != 1 {
			t.Fatalf("ran twice the same day: %d", runs())
		}
		e.clk.Add(24 * time.Hour)
		_ = e.rec.Tick(ctx)
		if runs() != 2 {
			t.Fatalf("next day: %d", runs())
		}
		// recon_time is a live setting: 03:15 on the 18th, moved to 23:30.
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "recon_time", json.RawMessage(`"23:30"`)); err != nil {
			t.Fatal(err)
		}
		_ = e.rec.Tick(ctx)
		if runs() != 2 {
			t.Fatalf("recon_time setting ignored: %d", runs())
		}
		e.clk.Add(21 * time.Hour) // 00:15 on the 19th: the 23:30 slot of the 18th is overdue
		_ = e.rec.Tick(ctx)
		if runs() != 3 {
			t.Fatalf("missed slot not caught up: %d", runs())
		}
	})
}

func TestResolveItemIsAudited(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.provider("stranger", paid("order_x", "pay_stranger", 100, "INR"))
		run, _ := e.rec.RunAccount(ctx, e.acct)
		it := e.items(run.ID)["unknown_provider_object:pay_stranger"]
		if err := e.rec.Resolve(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_1"}, org, it.ID, "test payment from the provider dashboard"); err != nil {
			t.Fatal(err)
		}
		var st, note string
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT state, note FROM recon_items WHERE id = ?", it.ID).Scan(&st, &note)
		})
		if st != "resolved" || note == "" || e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'recon.resolve' AND actor_id = 'usr_1'") != 1 {
			t.Errorf("state %q note %q", st, note)
		}
		if err := e.rec.Resolve(ctx, audit.System, org, it.ID, ""); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("a note is required: %v", err)
		}
		if err := e.rec.Resolve(ctx, audit.System, core.OrgScope{OrgID: "org_other"}, it.ID, "x"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-org resolve: %v", err)
		}
		// Once resolved, the same discrepancy opens a new item and alerts again.
		_, _ = e.rec.RunAccount(ctx, e.acct)
		if e.count("SELECT COUNT(*) FROM recon_items WHERE state = 'open'") != 1 {
			t.Error("a resolved discrepancy that recurs must reopen")
		}
	})
}
