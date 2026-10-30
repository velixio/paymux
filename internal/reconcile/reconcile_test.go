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

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/notify"
	"github.com/velixio/paymux/internal/reconcile"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
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
	t       *testing.T
	db      *store.DB
	core    *core.Service
	conns   *connectors.Service
	set     *settings.Service
	rec     *reconcile.Service
	fake    *fakeconn.State
	clk     *clock
	alerts  *alertLog
	scope   core.Scope
	acct    string
	mu      sync.Mutex
	events  []events.Event
	linked  []string
	linkErr error
	sink    *sinkRec
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
	e.sink = &sinkRec{}
	e.rec = reconcile.New(reconcile.Options{DB: db, Core: e.core, Conns: e.conns, Settings: e.set, Alerts: e.alerts, Loc: time.UTC, Now: clk.Now,
		KnownSubscription: func(_ context.Context, account, id string) bool { return account == pa.ID && id == "sub_known" },
		Subscriptions:     e.sink,
		LinkRenewal: func(_ context.Context, account string, pay connector.PaymentSnapshot) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.linked = append(e.linked, account+"/"+pay.ProviderPaymentID+"/"+pay.SubscriptionID)
			return e.linkErr
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

func renewal(id, sub string) connector.PaymentSnapshot {
	s := paid("", id, 100, "INR")
	s.SubscriptionID = sub
	return s
}

func TestUnknownProviderObjectAndRenewalLinking(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.provider("stranger", paid("order_x", "pay_stranger", 100, "INR"))
		e.provider("RENEWAL", renewal("pay_renew", "sub_known"))
		e.provider("OTHERSUB", renewal("pay_other", "sub_unknown")) // a subscription Paymux does not have: just unknown
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		items := e.items(run.ID)
		if items["unknown_provider_object:pay_stranger"].State != "open" || items["unknown_provider_object:pay_other"].State != "open" ||
			items["renewal_to_link:pay_renew"].State != "fixed" || run.Items != 3 || run.Fixed != 1 {
			t.Fatalf("items %+v run %+v", items, run)
		}
		if !slices.Equal(e.linked, []string{e.acct + "/pay_renew/sub_known"}) {
			t.Errorf("linked %v", e.linked)
		}
		k := e.alerts.kinds()
		slices.Sort(k)
		if !slices.Equal(k, []string{"recon.unknown_provider_object", "recon.unknown_provider_object"}) {
			t.Errorf("linked renewals do not alert, unknown payments do: %v", k)
		}
	})
}

func TestRenewalThatCannotBeLinkedStaysToLinkAndRetries(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.provider("RENEWAL", renewal("pay_renew", "sub_known"))
		e.linkErr = errors.New("subscriptions service down")
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		if it := e.items(run.ID)["renewal_to_link:pay_renew"]; it.State != "to_link" || run.Fixed != 0 {
			t.Fatalf("item %+v run %+v", it, run)
		}
		if len(e.alerts.kinds()) != 0 {
			t.Errorf("alerts %v", e.alerts.kinds())
		}
		// The next run links it and closes the earlier to_link item.
		e.linkErr = nil
		run2, _ := e.rec.RunAccount(ctx, e.acct)
		if e.items(run2.ID)["renewal_to_link:pay_renew"].State != "fixed" || len(e.linked) != 2 {
			t.Errorf("retry: %+v linked %v", e.items(run2.ID), e.linked)
		}
		if e.count("SELECT COUNT(*) FROM recon_items WHERE state = 'to_link'") != 0 {
			t.Error("the to_link item was left open after the renewal was linked")
		}
	})
}

func TestRenewalsAreLinkedWhenNoLinkerIsConfigured(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		rec := reconcile.New(reconcile.Options{DB: e.db, Core: e.core, Conns: e.conns, Settings: e.set, Alerts: e.alerts, Loc: time.UTC, Now: e.clk.Now,
			KnownSubscription: func(context.Context, string, string) bool { return true }})
		e.provider("RENEWAL", renewal("pay_renew", "sub_known"))
		run, err := rec.RunAccount(ctx, e.acct)
		if err != nil || e.items(run.ID)["renewal_to_link:pay_renew"].State != "to_link" || len(e.alerts.kinds()) != 0 {
			t.Errorf("%+v %v", e.items(run.ID), err)
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
		// The stray refund is a refund of one of our payments that the provider
		// made on its own (and whose webhook was missed): it is booked, not left open.
		if items["status_mismatch:rfnd_1"].State != "fixed" || items["unknown_provider_object:rfnd_stray"].State != "fixed" {
			t.Fatalf("items %+v", items)
		}
		got, _ := e.core.GetRefund(ctx, e.scope, r.ID)
		pay, _ := e.core.GetPaymentByID(ctx, p.ID)
		if got.Status != "succeeded" || got.ProviderRefundID != "rfnd_1" || pay.AmountRefunded != 10500 {
			t.Errorf("refund %+v payment %+v", got, pay)
		}
		var imported store.Refund
		_ = e.db.Read(ctx, func(q store.Querier) (err error) {
			imported, err = store.FindRefundByProviderID(ctx, q, a.ID, "rfnd_stray")
			return
		})
		if imported.InitiatedBy != "provider" || imported.Amount != 500 || imported.Status != "succeeded" {
			t.Errorf("imported refund %+v", imported)
		}
		if slices.Contains(e.alerts.kinds(), "recon.unknown_provider_object") {
			t.Errorf("a booked refund still alerted: %v", e.alerts.kinds())
		}
		// A second pass finds nothing to do and books nothing twice.
		if _, err := e.rec.RunAccount(ctx, e.acct); err != nil {
			t.Fatal(err)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 2 {
			t.Errorf("%d refunds after a second pass", e.count("SELECT COUNT(*) FROM refunds"))
		}
		if pay2, _ := e.core.GetPaymentByID(ctx, p.ID); pay2.AmountRefunded != 10500 {
			t.Errorf("refunded %d after a second pass", pay2.AmountRefunded)
		}
	})
}

func TestAProviderRefundOfSomeoneElsesPaymentStaysAnOpenItem(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, a := e.attempt("order_1")
		if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 50000, Currency: "INR", ProviderPaymentID: "pay_1"}); err != nil {
			t.Fatal(err)
		}
		e.provider("r1", paid("order_1", "pay_1", 50000, "INR"))
		e.fake.Locked(func() {
			e.fake.Refunds["stray"] = connector.RefundSnapshot{ProviderRefundID: "rfnd_stray", ProviderPaymentID: "pay_not_ours",
				Status: connector.RefundSucceeded, Amount: 500, Currency: "INR", CreatedAt: e.clk.Now().UnixMilli()}
		})
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.items(run.ID)["unknown_provider_object:rfnd_stray"].State; got != "open" {
			t.Fatalf("items %+v", e.items(run.ID))
		}
		if !slices.Contains(e.alerts.kinds(), "recon.unknown_provider_object") {
			t.Errorf("alerts %v", e.alerts.kinds())
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 0 {
			t.Error("a refund of someone else's payment was booked")
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

// ---- settlements ----

// capturedAttempt creates a captured attempt with provider payment id payID.
func (e *env) capturedAttempt(payID string, amount int64) (store.Payment, store.Attempt) {
	e.t.Helper()
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: amount, Currency: "INR"})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.core.CreateAttempt(ctx, res.Payment.ID, e.acct)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: amount, Currency: "INR", ProviderPaymentID: payID}); err != nil {
		e.t.Fatal(err)
	}
	p, _ := e.core.GetPaymentByID(ctx, res.Payment.ID)
	return p, a
}

func (e *env) settlement(s connector.SettlementSnapshot) {
	if s.SettledAt == 0 {
		s.SettledAt = e.clk.Now().Add(-2 * time.Hour).UnixMilli()
	}
	if s.Currency == "" {
		s.Currency = "INR"
	}
	e.fake.Locked(func() {
		for i, x := range e.fake.Settlements {
			if x.ProviderSettlementID == s.ProviderSettlementID {
				e.fake.Settlements[i] = s
				return
			}
		}
		e.fake.Settlements = append(e.fake.Settlements, s)
	})
}

type attemptFees struct {
	fee, tax, net int64
	settlement    string
}

func (e *env) fees(attemptID string) attemptFees {
	var f attemptFees
	var fee, tax, net *int64
	var sid *string
	_ = e.db.Read(ctx, func(q store.Querier) error {
		return q.QueryRowContext(ctx, "SELECT fee, fee_tax, net, settlement_id FROM payment_attempts WHERE id = ?", attemptID).Scan(&fee, &tax, &net, &sid)
	})
	if fee != nil {
		f.fee, f.tax, f.net = *fee, *tax, *net
	}
	if sid != nil {
		f.settlement = *sid
	}
	return f
}

func payLine(payID string, amount, fee, tax int64) connector.SettlementItem {
	return connector.SettlementItem{Kind: "payment", ProviderPaymentID: payID, Amount: amount, Fee: fee, Tax: tax}
}

func TestSettlementAttachesFeesAndWritesLedgerEntries(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p1, a1 := e.capturedAttempt("pay_1", 50000)
		_, a2 := e.capturedAttempt("pay_2", 30000)
		e.settlement(connector.SettlementSnapshot{ProviderSettlementID: "setl_1", Amount: 78000, Fees: 1500, Tax: 270, Status: "processed", UTR: "UTR1",
			Items: []connector.SettlementItem{payLine("pay_1", 50000, 900, 162), payLine("pay_2", 30000, 600, 108),
				{Kind: "refund", ProviderRefundID: "rfnd_x", Amount: -1000}}})
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "ok" || run.Items != 0 {
			t.Fatalf("run %+v", run)
		}
		var sid string
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT id FROM settlements WHERE provider_settlement_id = 'setl_1'").Scan(&sid)
		})
		if sid == "" || e.fees(a1.ID) != (attemptFees{900, 162, 50000 - 900 - 162, sid}) || e.fees(a2.ID) != (attemptFees{600, 108, 30000 - 600 - 108, sid}) {
			t.Fatalf("fees a1=%+v a2=%+v settlement %q", e.fees(a1.ID), e.fees(a2.ID), sid)
		}
		var row struct {
			amount, fees, tax, mismatch int64
			status, utr                 string
		}
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT amount, fees, fee_tax, fee_mismatch, status, utr FROM settlements WHERE id = ?", sid).
				Scan(&row.amount, &row.fees, &row.tax, &row.mismatch, &row.status, &row.utr)
		})
		if row.amount != 78000 || row.fees != 1500 || row.tax != 270 || row.mismatch != 0 || row.status != "processed" || row.utr != "UTR1" {
			t.Errorf("settlement row %+v", row)
		}
		var l []store.LedgerEntry
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { l, err = store.ListLedgerEntries(ctx, q, p1.ID); return })
		kinds := kindsOf(l)
		if !slices.Equal(kinds, []string{"charge", "fee", "fee_tax"}) {
			t.Fatalf("ledger of pay_1: %v", kinds)
		}
		for _, x := range l {
			switch x.Kind {
			case "fee":
				if x.Amount != -900 || x.Currency != "INR" {
					t.Errorf("fee entry %+v", x)
				}
			case "fee_tax":
				if x.Amount != -162 {
					t.Errorf("fee_tax entry %+v", x)
				}
			}
		}
		if e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'payout' AND settlement_id = ? AND amount = -78000 AND payment_id IS NULL", sid) != 1 {
			t.Error("payout entry missing")
		}
		// Reconciling again changes nothing.
		if _, err := e.rec.RunAccount(ctx, e.acct); err != nil {
			t.Fatal(err)
		}
		if e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind IN ('fee','fee_tax','payout')") != 5 || e.count("SELECT COUNT(*) FROM settlements") != 1 {
			t.Errorf("not idempotent: %d ledger entries", e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind IN ('fee','fee_tax','payout')"))
		}
		if len(e.alerts.kinds()) != 0 {
			t.Errorf("alerts %v", e.alerts.kinds())
		}
	})
}

func kindsOf(l []store.LedgerEntry) []string {
	var k []string
	for _, x := range l {
		k = append(k, x.Kind)
	}
	slices.Sort(k)
	return k
}

func TestSettlementFeeTotalsThatDifferAreFlagged(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.capturedAttempt("pay_1", 50000)
		e.settlement(connector.SettlementSnapshot{ProviderSettlementID: "setl_1", Amount: 49000, Fees: 1000, Tax: 180, Status: "processed",
			Items: []connector.SettlementItem{payLine("pay_1", 50000, 800, 144)}})
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		it := e.items(run.ID)["fee_mismatch:setl_1"]
		if it.State != "open" || run.Items != 1 {
			t.Fatalf("items %+v", e.items(run.ID))
		}
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"recon.fee_mismatch"}) {
			t.Errorf("alerts %v", k)
		}
		if e.count("SELECT COUNT(*) FROM settlements WHERE fee_mismatch = 1") != 1 {
			t.Error("settlement not marked")
		}
		// Found again: no new item, no new alert.
		_, _ = e.rec.RunAccount(ctx, e.acct)
		if e.count("SELECT COUNT(*) FROM recon_items WHERE kind = 'fee_mismatch'") != 1 || len(e.alerts.kinds()) != 1 {
			t.Error("fee mismatch duplicated")
		}
		// The provider corrects the settlement: the flag clears.
		e.settlement(connector.SettlementSnapshot{ProviderSettlementID: "setl_1", Amount: 49056, Fees: 800, Tax: 144, Status: "processed",
			Items: []connector.SettlementItem{payLine("pay_1", 50000, 800, 144)}})
		_, _ = e.rec.RunAccount(ctx, e.acct)
		if e.count("SELECT COUNT(*) FROM settlements WHERE fee_mismatch = 1") != 0 {
			t.Error("corrected settlement still flagged")
		}
	})
}

func TestSettlementWithUnknownPaymentsAndNoLines(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.capturedAttempt("pay_1", 50000)
		// One of the lines is a payment Paymux does not know: its fee cannot be attached, so the totals differ.
		e.settlement(connector.SettlementSnapshot{ProviderSettlementID: "setl_u", Amount: 1, Fees: 1500, Tax: 270, Status: "processed",
			Items: []connector.SettlementItem{payLine("pay_1", 50000, 900, 162), payLine("pay_other", 30000, 600, 108)}})
		// A settlement the connector reports without lines cannot be checked.
		e.settlement(connector.SettlementSnapshot{ProviderSettlementID: "setl_n", Amount: 5, Fees: 99, Tax: 9, Status: "processed"})
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		items := e.items(run.ID)
		if items["fee_mismatch:setl_u"].State != "open" || items["fee_mismatch:setl_n"].ProviderID != "" {
			t.Errorf("items %+v", items)
		}
		if e.count("SELECT COUNT(*) FROM settlements") != 2 {
			t.Error("both settlements are stored")
		}
	})
}

func TestPayoutIsRecordedOnceWhenTheSettlementIsProcessed(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.capturedAttempt("pay_1", 50000)
		s := connector.SettlementSnapshot{ProviderSettlementID: "setl_1", Amount: 49000, Fees: 900, Tax: 100, Status: "created",
			Items: []connector.SettlementItem{payLine("pay_1", 50000, 900, 100)}}
		e.settlement(s)
		_, _ = e.rec.RunAccount(ctx, e.acct)
		if e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'payout'") != 0 {
			t.Fatal("payout recorded before the settlement was processed")
		}
		s.Status = "processed"
		e.settlement(s)
		for i := 0; i < 2; i++ {
			_, _ = e.rec.RunAccount(ctx, e.acct)
		}
		if e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'payout'") != 1 || e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'fee'") != 1 {
			t.Errorf("payouts=%d fees=%d", e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'payout'"), e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'fee'"))
		}
	})
}

func TestSettlementListingFailureFailsTheRun(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.fake.Locked(func() { e.fake.SettleErr = connector.ErrTemporary })
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err == nil || run.Status != "failed" || !slices.Contains(e.alerts.kinds(), "recon.failed") {
			t.Errorf("run %+v err %v alerts %v", run, err, e.alerts.kinds())
		}
	})
}

// ---- mandates and provider subscriptions ----

// sinkRec stands in for the subscriptions service: it records the
// notifications reconciliation hands over.
type sinkRec struct {
	mu    sync.Mutex
	notes []connector.Notification
	err   error
}

func (s *sinkRec) HandleNotification(_ context.Context, account string, n connector.Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, n)
	return s.err
}

func (s *sinkRec) calls() []connector.Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]connector.Notification(nil), s.notes...)
}

func (e *env) mandate(id, providerID, status string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertMandate(ctx, tx, store.Mandate{ID: id, OrgID: store.DefaultOrgID, Mode: "test", AppID: "app_1", CustomerID: "cus_1",
			ProviderAccountID: e.acct, ProviderRef: "MREF" + id, ProviderMandateID: providerID, Method: "upi", Status: status, MaxAmount: 150000,
			Currency: "INR", Frequency: "monthly", CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) subscription(id, providerID, status, billing string) {
	e.t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		return store.InsertSubscription(ctx, tx, store.Subscription{ID: id, OrgID: store.DefaultOrgID, Mode: "test", AppID: "app_1", CustomerID: "cus_1",
			Status: status, BillingMode: billing, ProviderAccountID: e.acct, ProviderSubscriptionID: providerID, ProviderRef: "SREF" + id, Amount: 49900,
			Currency: "INR", Interval: "month", IntervalCount: 1, AnchorAt: e.clk.Now(), CurrentPeriodStart: e.clk.Now(), CurrentPeriodEnd: e.clk.Now().Add(720 * time.Hour),
			CheckoutTokenHash: "h", ExpiresAt: e.clk.Now().Add(time.Hour), Version: 1, CreatedAt: e.clk.Now(), UpdatedAt: e.clk.Now()})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestMandateStatusMismatchesAreAppliedThroughTheSubscriptionsService(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.mandate("mdt_pending", "pm_1", "pending") // provider says active
		e.mandate("mdt_same", "pm_2", "active")     // in sync
		e.mandate("mdt_revoked", "pm_3", "active")  // provider says revoked
		e.mandate("mdt_gone", "pm_4", "active")     // the provider does not know it
		e.mandate("mdt_final", "pm_5", "revoked")   // final: never fetched
		e.mandate("mdt_nopid", "", "pending")       // not yet at the provider
		e.fake.Locked(func() {
			e.fake.Mandates["pm_1"] = connector.MandateSnapshot{ProviderMandateID: "pm_1", Status: "active", RawStatus: "confirmed", MaxAmount: 150000, Currency: "INR"}
			e.fake.Mandates["pm_2"] = connector.MandateSnapshot{ProviderMandateID: "pm_2", Status: "active"}
			e.fake.Mandates["pm_3"] = connector.MandateSnapshot{ProviderMandateID: "pm_3", Status: "revoked", RawStatus: "cancelled"}
			e.fake.Mandates["pm_5"] = connector.MandateSnapshot{ProviderMandateID: "pm_5", Status: "active"}
		})
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		notes := e.sink.calls()
		if len(notes) != 2 {
			t.Fatalf("notifications %+v", notes)
		}
		got := map[string]string{}
		for _, n := range notes {
			if n.Kind != connector.NotifyMandate || n.Mandate == nil {
				t.Fatalf("notification %+v", n)
			}
			got[n.Mandate.ProviderMandateID] = n.Mandate.Status
		}
		if got["pm_1"] != "active" || got["pm_3"] != "revoked" {
			t.Errorf("applied %v", got)
		}
		items := e.items(run.ID)
		if items["status_mismatch:pm_1"].State != "fixed" || items["status_mismatch:pm_3"].State != "fixed" || run.Fixed != 2 || run.Items != 2 {
			t.Errorf("run %+v items %+v", run, items)
		}
		e.fake.Locked(func() {
			fetched := append([]string(nil), e.fake.MandateFetches...)
			slices.Sort(fetched)
			if !slices.Equal(fetched, []string{"pm_1", "pm_2", "pm_3", "pm_4"}) {
				t.Errorf("fetched %v", fetched)
			}
		})
		if len(e.alerts.kinds()) != 0 {
			t.Errorf("alerts %v", e.alerts.kinds())
		}
	})
}

func TestProviderSubscriptionStatusMismatches(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.subscription("sub_incomplete", "ps_1", "incomplete", "provider_managed") // provider: active
		e.subscription("sub_active_ok", "ps_2", "active", "provider_managed")      // provider: active
		e.subscription("sub_canceled", "ps_3", "active", "provider_managed")       // provider: canceled
		e.subscription("sub_pastdue", "ps_4", "active", "provider_managed")        // provider: past_due
		e.subscription("sub_ended", "ps_5", "ended", "provider_managed")           // final: never fetched
		e.subscription("sub_merchant", "ps_6", "active", "merchant_triggered")     // ours to trigger: provider has nothing to say
		e.subscription("sub_trial", "ps_7", "trialing", "provider_managed")        // provider "active" while trialing is not a mismatch
		e.fake.Locked(func() {
			for id, st := range map[string]string{"ps_1": "active", "ps_2": "active", "ps_3": "canceled", "ps_4": "past_due", "ps_6": "canceled", "ps_7": "active"} {
				e.fake.Subs[id] = connector.SubscriptionSnapshot{ProviderSubscriptionID: id, Status: st, RawStatus: st, Amount: 49900, Currency: "INR"}
			}
		})
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, n := range e.sink.calls() {
			if n.Kind != connector.NotifySubscription || n.Subscription == nil || n.Payment != nil {
				t.Fatalf("notification %+v", n)
			}
			got[n.Subscription.ProviderSubscriptionID] = n.Subscription.Status
		}
		want := map[string]string{"ps_1": "active", "ps_3": "canceled", "ps_4": "past_due"}
		if len(got) != 3 || got["ps_1"] != want["ps_1"] || got["ps_3"] != want["ps_3"] || got["ps_4"] != want["ps_4"] {
			t.Errorf("applied %v want %v", got, want)
		}
		if run.Fixed != 3 || e.items(run.ID)["status_mismatch:ps_3"].State != "fixed" {
			t.Errorf("run %+v items %+v", run, e.items(run.ID))
		}
		e.fake.Locked(func() {
			for _, id := range e.fake.SubFetches {
				if id == "ps_5" || id == "ps_6" {
					t.Errorf("fetched %s, which is final or not provider managed", id)
				}
			}
		})
	})
}

func TestMandateAndSubscriptionFailuresAreAlertedNotFatal(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.mandate("mdt_1", "pm_1", "pending")
		e.fake.Locked(func() {
			e.fake.Mandates["pm_1"] = connector.MandateSnapshot{ProviderMandateID: "pm_1", Status: "active"}
		})
		e.sink.err = errors.New("subscriptions service down")
		run, err := e.rec.RunAccount(ctx, e.acct)
		if err != nil || run.Status != "ok" {
			t.Fatalf("a failing apply must not fail the run: %+v %v", run, err)
		}
		if it := e.items(run.ID)["status_mismatch:pm_1"]; it.State != "open" || run.Fixed != 0 {
			t.Errorf("item %+v", it)
		}
		if k := e.alerts.kinds(); !slices.Equal(k, []string{"recon.autofix_failed"}) {
			t.Errorf("alerts %v", k)
		}
		// A provider outage while fetching is logged and skipped.
		e.sink.err = nil
		e.fake.Locked(func() { e.fake.ManagerErr = connector.ErrTemporary })
		if run, err := e.rec.RunAccount(ctx, e.acct); err != nil || run.Status != "ok" {
			t.Errorf("fetch outage: %+v %v", run, err)
		}
	})
}

func TestNoSubscriptionsServiceMeansMandatesAreNotChecked(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		rec := reconcile.New(reconcile.Options{DB: e.db, Core: e.core, Conns: e.conns, Settings: e.set, Alerts: e.alerts, Loc: time.UTC, Now: e.clk.Now})
		e.mandate("mdt_1", "pm_1", "pending")
		e.fake.Locked(func() {
			e.fake.Mandates["pm_1"] = connector.MandateSnapshot{ProviderMandateID: "pm_1", Status: "active"}
		})
		if _, err := rec.RunAccount(ctx, e.acct); err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			if len(e.fake.MandateFetches) != 0 {
				t.Error("fetched mandates without a service to apply them")
			}
		})
	})
}

// H-13. A settlement in another currency than the attempt it lists is not
// booked against it: a fee in USD must not become a fee on an INR payment.
func TestSettlementInAnotherCurrencyIsNotBooked(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, a := e.capturedAttempt("pay_1", 50000)
		e.settlement(connector.SettlementSnapshot{ProviderSettlementID: "setl_usd", Currency: "USD", Amount: 49000, Fees: 800, Tax: 144, Status: "processed",
			Items: []connector.SettlementItem{payLine("pay_1", 50000, 800, 144)}})
		if _, err := e.rec.RunAccount(ctx, e.acct); err != nil {
			t.Fatal(err)
		}
		if f := e.fees(a.ID); f.settlement != "" || f.fee != 0 {
			t.Errorf("a USD settlement was attached to an INR attempt: %+v", f)
		}
		if n := e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind IN ('fee','fee_tax') AND currency <> 'INR'"); n != 0 {
			t.Errorf("%d fee entries in the wrong currency", n)
		}
	})
}
