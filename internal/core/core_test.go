package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

var ctx = context.Background()

type env struct {
	t        *testing.T
	db       *store.DB
	svc      *core.Service
	q        *jobs.Queue
	set      *settings.Service
	scope    core.Scope
	pa       string
	mu       sync.Mutex
	events   []events.Event
	failEmit bool
	alerts   alertRec
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

func (e *env) lastEvent() events.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.events[len(e.events)-1]
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	e := &env{t: t, db: db, set: settings.New(db)}
	e.q = jobs.New(db, jobs.Options{Logger: nil})
	e.svc = core.NewService(core.Options{
		DB: db, Settings: e.set, Jobs: e.q, Alerts: &e.alerts,
		Emitter: events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.failEmit {
				return errors.New("emit failed")
			}
			e.events = append(e.events, ev)
			return nil
		}),
	})
	now := time.Now()
	err := db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", now); err != nil {
			return err
		}
		if err := store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a",
			DefaultReturnURL: "https://app.example.com/return", CreatedAt: now}); err != nil {
			return err
		}
		return store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "test",
			Connector: "fake", Name: "Fake", Status: "active", CreatedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
		t.Fatal(err)
	}
	e.scope = core.Scope{AppID: "app_1", Mode: "test"}
	e.pa = "pa_1"
	return e
}

func (e *env) payment(amount int64) store.Payment {
	e.t.Helper()
	res, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: amount, Currency: "INR", OrderRef: "ord-1"})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Payment
}

func (e *env) attempt(paymentID string) store.Attempt {
	e.t.Helper()
	a, err := e.svc.CreateAttempt(ctx, paymentID, e.pa)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) apply(attemptID string, snap core.Snapshot) core.ApplyResult {
	e.t.Helper()
	r, err := e.svc.ApplyAttemptSnapshot(ctx, attemptID, snap)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func captured(amount int64) core.Snapshot {
	return core.Snapshot{Status: state.AttemptCaptured, Amount: amount, Currency: "INR", Method: "upi", ProviderPaymentID: "pp_1"}
}

func TestAttemptStoresSafeMethodDetails(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		a := e.attempt(p.ID)
		international := false
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 100, Currency: "INR", Method: "card", ProviderPaymentID: "pay_card"})
		result := e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 100, Currency: "INR", Method: "card",
			ProviderPaymentID: "pay_card", MethodDetails: connector.PaymentMethodDetails{Card: &connector.CardDetails{
				Last4: "5000", Network: "American Express", Funding: "credit", International: &international}}})
		if !result.Changed {
			t.Fatal("a repeated snapshot with new method details was not saved")
		}
		views, err := e.svc.PaymentAttempts(ctx, e.scope, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 || views[0].MethodDetails == nil || views[0].MethodDetails.Card == nil ||
			views[0].MethodDetails.Card.Last4 != "5000" || views[0].MethodDetails.Card.Network != "American Express" {
			t.Fatalf("attempt views = %+v", views)
		}
	})
}

func (e *env) get(id string) store.Payment {
	e.t.Helper()
	p, err := e.svc.GetPayment(ctx, e.scope, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) })
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func each(t *testing.T, fn func(t *testing.T, e *env)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newEnv(t, db)) })
}

func TestCreatePayment(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		before := time.Now()
		res, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{
			Scope: e.scope, Amount: 49900, Currency: "inr", OrderRef: "ord-9", Description: "Pro plan",
			Metadata: map[string]string{"plan": "pro"},
			Customer: &core.CustomerInput{ExternalRef: "u-1", Name: "Asha", Email: "asha@example.com",
				Billing: store.Billing{StateCode: "29", Country: "IN"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		p := res.Payment
		if p.Status != "created" || p.Version != 1 || p.Currency != "INR" || p.Amount != 49900 || p.Mode != "test" ||
			p.OrgID != store.DefaultOrgID || p.Kind != "one_time" || p.ReturnURL != "https://app.example.com/return" ||
			p.Metadata["plan"] != "pro" || p.CustomerID == "" {
			t.Fatalf("payment %+v", p)
		}
		if d := p.ExpiresAt.Sub(before); d < 59*time.Minute || d > 61*time.Minute {
			t.Errorf("expires_at %v is not about 60m ahead", d)
		}
		if res.ClientSecret == "" || p.ClientSecretHash == "" || p.ClientSecretHash == res.ClientSecret {
			t.Errorf("secret handling: secret=%q hash=%q", res.ClientSecret, p.ClientSecretHash)
		}
		if !core.VerifyClientSecret(p, res.ClientSecret, before) || core.VerifyClientSecret(p, res.ClientSecret+"x", before) ||
			core.VerifyClientSecret(p, res.ClientSecret, p.ExpiresAt.Add(time.Second)) {
			t.Error("client secret verification wrong")
		}
		if got := e.emitted(); !slices.Equal(got, []string{"payment.created"}) {
			t.Errorf("events %v", got)
		}
		ev := e.lastEvent()
		if ev.ObjectID != p.ID || ev.ObjectVersion != 1 || ev.AppID != "app_1" || ev.Mode != "test" || ev.OrgID != store.DefaultOrgID {
			t.Errorf("event %+v", ev)
		}
		raw, _ := json.Marshal(ev.Data)
		if !json.Valid(raw) || len(raw) < 10 {
			t.Errorf("event data %s", raw)
		}
		again, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR",
			Customer: &core.CustomerInput{ExternalRef: "u-1", Name: "Asha K"}})
		if err != nil || again.Payment.CustomerID != p.CustomerID {
			t.Errorf("customer not reused by external_ref: %v %v", again.Payment.CustomerID, err)
		}
		if e.count("SELECT COUNT(*) FROM customers") != 1 {
			t.Error("customer duplicated")
		}
	})
}

func TestCreatePaymentHonoursTTLSetting(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "payment_ttl", json.RawMessage(`15`)); err != nil {
			t.Fatal(err)
		}
		before := time.Now()
		res, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR"})
		if err != nil {
			t.Fatal(err)
		}
		if d := res.Payment.ExpiresAt.Sub(before); d < 14*time.Minute || d > 16*time.Minute {
			t.Errorf("ttl %v", d)
		}
	})
}

func TestCreatePaymentValidation(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		many := map[string]string{}
		for i := 0; i < 21; i++ {
			many[string(rune('a'+i))] = "v"
		}
		cases := map[string]core.CreatePaymentInput{
			"zero amount":      {Amount: 0, Currency: "INR"},
			"negative amount":  {Amount: -5, Currency: "INR"},
			"unknown currency": {Amount: 100, Currency: "XXX"},
			"empty currency":   {Amount: 100},
			"too much meta":    {Amount: 100, Currency: "INR", Metadata: many},
			"bad return url":   {Amount: 100, Currency: "INR", ReturnURL: "javascript:alert(1)"},
			"relative return":  {Amount: 100, Currency: "INR", ReturnURL: "/back"},
			"bad email":        {Amount: 100, Currency: "INR", Customer: &core.CustomerInput{Email: "nope"}},
		}
		for name, in := range cases {
			in.Scope = e.scope
			_, err := e.svc.CreatePayment(ctx, in)
			if !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%s: want ErrInvalid, got %v", name, err)
			}
		}
		if _, err := e.svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: core.Scope{AppID: "app_x", Mode: "test"}, Amount: 1, Currency: "INR"}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("unknown app: %v", err)
		}
		if e.count("SELECT COUNT(*) FROM payments") != 0 {
			t.Error("invalid input created rows")
		}
	})
}

func TestGetPaymentIsScoped(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		for _, sc := range []core.Scope{{AppID: "app_2", Mode: "test"}, {AppID: "app_1", Mode: "live"}} {
			if _, err := e.svc.GetPayment(ctx, sc, p.ID); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("scope %+v: %v", sc, err)
			}
		}
		if _, err := e.svc.GetPayment(ctx, e.scope, "pay_missing"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
	})
}

func TestListPaymentsPaginates(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		var ids []string
		for i := 0; i < 5; i++ {
			ids = append(ids, e.payment(int64(100+i)).ID)
			time.Sleep(2 * time.Millisecond) // ids order by millisecond
		}
		slices.Reverse(ids) // newest first
		var got []string
		cursor := ""
		pages := 0
		for {
			page, err := e.svc.ListPayments(ctx, e.scope, core.ListParams{Limit: 2, After: cursor})
			if err != nil {
				t.Fatal(err)
			}
			pages++
			for _, p := range page.Payments {
				got = append(got, p.ID)
			}
			if !page.HasMore {
				if page.NextCursor != "" {
					t.Error("cursor on last page")
				}
				break
			}
			cursor = page.NextCursor
		}
		if pages != 3 || !slices.Equal(got, ids) {
			t.Fatalf("pages=%d got=%v want=%v", pages, got, ids)
		}
		if _, err := e.svc.ListPayments(ctx, e.scope, core.ListParams{Limit: 1000}); err != nil {
			t.Errorf("large limit must be clamped, not rejected: %v", err)
		}
		if _, err := e.svc.ListPayments(ctx, e.scope, core.ListParams{Status: "bogus"}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("bogus status: %v", err)
		}
		if page, _ := e.svc.ListPayments(ctx, e.scope, core.ListParams{Status: "succeeded"}); len(page.Payments) != 0 {
			t.Error("status filter ignored")
		}
	})
}

func TestCancel(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		got, err := e.svc.CancelPayment(ctx, e.scope, audit.Actor{Type: audit.ActorAPIKey, ID: "key_1"}, p.ID)
		if err != nil || got.Status != "canceled" || got.Version != 2 {
			t.Fatalf("cancel: %+v %v", got, err)
		}
		if _, err := e.svc.CancelPayment(ctx, e.scope, audit.System, p.ID); err != nil {
			t.Errorf("second cancel must be idempotent: %v", err)
		}
		if ev := e.emitted(); !slices.Equal(ev, []string{"payment.created", "payment.canceled"}) {
			t.Errorf("events %v", ev)
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'payment.cancel'") != 1 {
			t.Error("cancel not audited exactly once")
		}
		paid := e.payment(100)
		e.apply(e.attempt(paid.ID).ID, captured(100))
		if _, err := e.svc.CancelPayment(ctx, e.scope, audit.System, paid.ID); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("cancel succeeded payment: %v", err)
		}
		if _, err := e.svc.CancelPayment(ctx, core.Scope{AppID: "app_2", Mode: "test"}, audit.System, p.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-app cancel: %v", err)
		}
	})
}

func TestSnapshotHappyPath(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a := e.attempt(p.ID)
		if a.Status != "created" || len(a.ProviderRef) != 20 {
			t.Fatalf("attempt %+v", a)
		}
		r := e.apply(a.ID, core.Snapshot{Status: state.AttemptPending})
		if r.Payment.Status != "requires_action" || r.Payment.Version != 2 || !r.Changed {
			t.Fatalf("pending: %+v", r.Payment)
		}
		r = e.apply(a.ID, core.Snapshot{Status: state.AttemptAuthorized})
		if r.Payment.Status != "processing" {
			t.Fatalf("authorized: %+v", r.Payment)
		}
		r = e.apply(a.ID, captured(500))
		got := r.Payment
		if got.Status != "succeeded" || got.AmountReceived != 500 || got.Version != 4 || r.Attempt.Status != "captured" ||
			r.Attempt.AmountReceived != 500 || r.Attempt.Method != "upi" || r.Attempt.ProviderPaymentID != "pp_1" {
			t.Fatalf("captured: payment=%+v attempt=%+v", got, r.Attempt)
		}
		want := []string{"payment.created", "payment.requires_action", "payment.processing", "payment.succeeded"}
		if ev := e.emitted(); !slices.Equal(ev, want) {
			t.Errorf("events %v want %v", ev, want)
		}
		if last := e.lastEvent(); last.ObjectVersion != 4 || last.ObjectID != p.ID {
			t.Errorf("last event %+v", last)
		}
		var ledger []store.LedgerEntry
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { ledger, err = store.ListLedgerEntries(ctx, q, p.ID); return })
		if len(ledger) != 1 || ledger[0].Kind != "charge" || ledger[0].Amount != 500 || ledger[0].Currency != "INR" || ledger[0].Mode != "test" {
			t.Errorf("ledger %+v", ledger)
		}
		// Replaying the same capture changes nothing.
		r = e.apply(a.ID, captured(500))
		if r.Changed || r.Payment.Version != 4 || len(e.emitted()) != 4 || e.count("SELECT COUNT(*) FROM ledger_entries") != 1 {
			t.Errorf("replay changed state: %+v events=%v", r.Payment, e.emitted())
		}
	})
}

func TestSnapshotIsAtomic(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a := e.attempt(p.ID)
		e.failEmit = true
		if _, err := e.svc.ApplyAttemptSnapshot(ctx, a.ID, captured(500)); err == nil {
			t.Fatal("expected emitter failure to surface")
		}
		e.failEmit = false
		got := e.get(p.ID)
		att, _ := e.svc.GetAttempt(ctx, a.ID)
		if got.Status != "created" || got.Version != 1 || got.AmountReceived != 0 || att.Status != "created" ||
			e.count("SELECT COUNT(*) FROM ledger_entries") != 0 {
			t.Fatalf("partial write survived: payment=%+v attempt=%+v", got, att)
		}
	})
}

func TestSnapshotMismatchNeedsReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		for name, snap := range map[string]core.Snapshot{
			"amount":   {Status: state.AttemptCaptured, Amount: 499, Currency: "INR"},
			"currency": {Status: state.AttemptCaptured, Amount: 500, Currency: "USD"},
		} {
			p := e.payment(500)
			r := e.apply(e.attempt(p.ID).ID, snap)
			if r.Payment.Status != "needs_review" || !r.Payment.HasFlag("amount_mismatch") || r.Payment.AmountReceived != snap.Amount {
				t.Errorf("%s: %+v", name, r.Payment)
			}
			if e.lastEvent().Type != "payment.needs_review" {
				t.Errorf("%s: last event %s", name, e.lastEvent().Type)
			}
		}
	})
}

func TestSnapshotLateSuccess(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a := e.attempt(p.ID)
		r := e.apply(a.ID, core.Snapshot{Status: state.AttemptFailed, FailureCode: "bank_declined", FailureMessage: "no"})
		if r.Payment.Status != "failed" || r.Attempt.FailureCode != "bank_declined" {
			t.Fatalf("failed: %+v", r)
		}
		r = e.apply(a.ID, captured(500))
		if r.Payment.Status != "succeeded" || !r.Payment.HasFlag("late_success") || r.Payment.AmountReceived != 500 {
			t.Fatalf("late success: %+v", r.Payment)
		}
		if ev := e.emitted(); ev[len(ev)-2] != "payment.failed" || ev[len(ev)-1] != "payment.succeeded" {
			t.Errorf("events %v", ev)
		}
	})
}

func TestSnapshotFailureWithOtherOpenAttemptKeepsPaymentOpen(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, core.Snapshot{Status: state.AttemptPending})
		r := e.apply(a2.ID, core.Snapshot{Status: state.AttemptFailed})
		if r.Payment.Status != "requires_action" {
			t.Fatalf("payment failed while another attempt is open: %s", r.Payment.Status)
		}
		r = e.apply(a1.ID, core.Snapshot{Status: state.AttemptFailed})
		if r.Payment.Status != "failed" {
			t.Fatalf("last open attempt failed: %s", r.Payment.Status)
		}
	})
}

func TestSnapshotSuccessAfterCancelNeedsReviewNeverRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "duplicate_payment_action", json.RawMessage(`"auto_refund"`)); err != nil {
			t.Fatal(err)
		}
		p := e.payment(500)
		a := e.attempt(p.ID)
		if _, err := e.svc.CancelPayment(ctx, e.scope, audit.System, p.ID); err != nil {
			t.Fatal(err)
		}
		r := e.apply(a.ID, captured(500))
		if r.Payment.Status != "needs_review" || !r.Payment.HasFlag("success_after_cancel") || !r.Payment.HasFlag("late_success") {
			t.Fatalf("%+v", r.Payment)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 0 || e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit'") != 0 {
			t.Error("success after cancel must never be auto-refunded")
		}
	})
}

func TestDoubleSuccessReview(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, captured(500))
		r := e.apply(a2.ID, captured(500))
		got := r.Payment
		if got.Status != "succeeded" || !got.HasFlag("duplicate_success") || got.AmountReceived != 500 || !r.Attempt.Duplicate {
			t.Fatalf("payment=%+v attempt=%+v", got, r.Attempt)
		}
		if last := e.lastEvent(); last.Type != "payment.needs_review" || last.ObjectVersion != got.Version {
			t.Errorf("last event %+v", last)
		}
		if e.count("SELECT COUNT(*) FROM refunds") != 0 || e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit'") != 0 {
			t.Error("review mode must not create refunds")
		}
		if e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'charge'") != 2 {
			t.Error("both charges must hit the ledger")
		}
		// Replaying the duplicate is a no-op.
		before := len(e.emitted())
		e.apply(a2.ID, captured(500))
		if len(e.emitted()) != before {
			t.Error("replay of a duplicate emitted again")
		}
	})
}

func TestDoubleSuccessAutoRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "duplicate_payment_action", json.RawMessage(`"auto_refund"`)); err != nil {
			t.Fatal(err)
		}
		p := e.payment(500)
		a1, a2 := e.attempt(p.ID), e.attempt(p.ID)
		e.apply(a1.ID, captured(500))
		r := e.apply(a2.ID, captured(500))
		if r.Payment.Status != "succeeded" || !r.Payment.HasFlag("duplicate_success") {
			t.Fatalf("payment %+v", r.Payment)
		}
		var refunds []store.Refund
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { refunds, err = store.ListRefunds(ctx, q, p.ID); return })
		if len(refunds) != 1 {
			t.Fatalf("refunds %+v", refunds)
		}
		rf := refunds[0]
		if rf.AttemptID != a2.ID || rf.Amount != 500 || rf.Currency != "INR" || rf.Status != "pending" ||
			rf.InitiatedBy != "system" || rf.Reason != "duplicate_payment" || len(rf.ProviderRef) != 20 || rf.AppID != "app_1" {
			t.Errorf("refund %+v", rf)
		}
		if e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit' AND state = 'pending'") != 1 {
			t.Error("refund.submit not enqueued")
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'refund.create' AND actor_type = 'system'") != 1 {
			t.Error("auto refund not audited")
		}
		if ev := e.emitted(); ev[len(ev)-1] != "refund.created" {
			t.Errorf("events %v", ev)
		}
		// The first attempt stays fully refundable only by an explicit request;
		// the extra attempt has nothing left.
		_, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "dashboard",
			PaymentID: p.ID, AttemptID: a2.ID, Amount: 1})
		if !errors.Is(err, core.ErrRefundExceeds) {
			t.Errorf("extra attempt double refund: %v", err)
		}
		// Replaying the duplicate success must not create a second refund.
		e.apply(a2.ID, captured(500))
		if e.count("SELECT COUNT(*) FROM refunds") != 1 {
			t.Error("duplicate refund created on replay")
		}
	})
}

func TestSnapshotRejectsBadInput(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(500)
		a := e.attempt(p.ID)
		for name, s := range map[string]core.Snapshot{
			"bad status":           {Status: "weird"},
			"capture no amount":    {Status: state.AttemptCaptured, Currency: "INR"},
			"capture no currency":  {Status: state.AttemptCaptured, Amount: 500},
			"capture bad currency": {Status: state.AttemptCaptured, Amount: 500, Currency: "ZZZ"},
		} {
			if _, err := e.svc.ApplyAttemptSnapshot(ctx, a.ID, s); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := e.svc.ApplyAttemptSnapshot(ctx, "att_missing", captured(1)); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing attempt: %v", err)
		}
		if _, err := e.svc.CreateAttempt(ctx, p.ID, "pa_missing"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing provider account: %v", err)
		}
		e.apply(a.ID, captured(500))
		if _, err := e.svc.CreateAttempt(ctx, p.ID, e.pa); err != nil {
			t.Errorf("extra attempt on a succeeded payment must be allowed (double pay): %v", err)
		}
	})
}

func TestCreateRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(1000)
		a := e.attempt(p.ID)
		in := core.CreateRefundInput{Scope: e.scope, Actor: audit.Actor{Type: audit.ActorAPIKey, ID: "key_1"}, InitiatedBy: "api", PaymentID: p.ID, Reason: "customer request"}
		if _, err := e.svc.CreateRefund(ctx, in); !errors.Is(err, core.ErrNotRefundable) {
			t.Fatalf("refund before success: %v", err)
		}
		e.apply(a.ID, captured(1000))

		in.Amount = 300
		r1, err := e.svc.CreateRefund(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if r1.Amount != 300 || r1.Status != "pending" || r1.AttemptID != a.ID || r1.InitiatedBy != "api" ||
			r1.Currency != "INR" || len(r1.ProviderRef) != 20 || r1.Reason != "customer request" || r1.Actor != "key_1" {
			t.Fatalf("refund %+v", r1)
		}
		in.Amount = 800
		if _, err := e.svc.CreateRefund(ctx, in); !errors.Is(err, core.ErrRefundExceeds) {
			t.Fatalf("over-refund: %v", err)
		}
		in.Amount = 0 // default: everything left
		r2, err := e.svc.CreateRefund(ctx, in)
		if err != nil || r2.Amount != 700 {
			t.Fatalf("default amount: %+v %v", r2, err)
		}
		in.Amount = 1
		if _, err := e.svc.CreateRefund(ctx, in); !errors.Is(err, core.ErrRefundExceeds) {
			t.Fatalf("fully refunded: %v", err)
		}
		in.Amount = -1
		if _, err := e.svc.CreateRefund(ctx, in); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("negative: %v", err)
		}
		if n := e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit'"); n != 2 {
			t.Errorf("jobs=%d", n)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "refund.created" {
			t.Errorf("events %v", ev)
		}
		got, err := e.svc.GetRefund(ctx, e.scope, r1.ID)
		if err != nil || got.ID != r1.ID {
			t.Errorf("GetRefund %+v %v", got, err)
		}
		if _, err := e.svc.GetRefund(ctx, core.Scope{AppID: "app_2", Mode: "test"}, r1.ID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-app GetRefund: %v", err)
		}
		if _, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: core.Scope{AppID: "app_2", Mode: "test"}, PaymentID: p.ID, InitiatedBy: "api"}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-app refund: %v", err)
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'refund.create'") != 2 {
			t.Error("refunds not audited")
		}
	})
}

func TestCreateRefundExplicitAttemptValidation(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(1000)
		other := e.payment(1000)
		a, oa := e.attempt(p.ID), e.attempt(other.ID)
		e.apply(a.ID, captured(1000))
		e.apply(oa.ID, captured(1000))
		in := core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "dashboard", PaymentID: p.ID, AttemptID: oa.ID}
		if _, err := e.svc.CreateRefund(ctx, in); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("attempt of another payment: %v", err)
		}
		pending := e.attempt(p.ID)
		in.AttemptID = pending.ID
		if _, err := e.svc.CreateRefund(ctx, in); !errors.Is(err, core.ErrNotRefundable) {
			t.Errorf("uncaptured attempt: %v", err)
		}
	})
}

func TestCreateRefundConcurrentCannotOverRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		e.apply(e.attempt(p.ID).ID, captured(100))
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok, exceeded := 0, 0
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 40})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					ok++
				case errors.Is(err, core.ErrRefundExceeds):
					exceeded++
				default:
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		wg.Wait()
		if ok != 2 || exceeded != 4 {
			t.Fatalf("ok=%d exceeded=%d", ok, exceeded)
		}
		var sum int
		_ = e.db.Read(ctx, func(q store.Querier) error {
			return q.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount),0) FROM refunds").Scan(&sum)
		})
		if sum != 80 {
			t.Fatalf("refunded %d of 100", sum)
		}
	})
}

func TestRefundSubmitJobHandlerIsRegistered(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		core.RegisterJobs(e.q)
		p := e.payment(100)
		e.apply(e.attempt(p.ID).ID, captured(100))
		if _, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID}); err != nil {
			t.Fatal(err)
		}
		n, err := e.q.RunOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("RunOnce n=%d err=%v", n, err)
		}
		if e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'refund.submit' AND state = 'done'") != 1 {
			t.Error("refund.submit did not complete")
		}
	})
}
