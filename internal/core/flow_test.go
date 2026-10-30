package core_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/domains"
	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/store"
)

type routerFunc func(ctx context.Context, q store.Queryer, req core.RouteRequest) error

func (f routerFunc) CheckRoute(ctx context.Context, q store.Queryer, req core.RouteRequest) error {
	return f(ctx, q, req)
}

func TestCreatePaymentConsultsRouter(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		var seen core.RouteRequest
		svc := core.NewService(core.Options{DB: e.db, Settings: e.set, Jobs: e.q,
			Router: routerFunc(func(_ context.Context, _ store.Queryer, req core.RouteRequest) error {
				seen = req
				if req.Currency == "USD" {
					return core.NewError(core.ErrNoRoute, "no route for USD")
				}
				return nil
			})})
		_, err := svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 500, Currency: "inr", Country: "IN",
			AllowedConnectors: []string{"razorpay"}, Metadata: map[string]string{"plan": "pro"}})
		if err != nil {
			t.Fatal(err)
		}
		if seen.Currency != "INR" || seen.Amount != 500 || seen.Country != "IN" || seen.Scope != e.scope || seen.Purpose != "one_time" ||
			!slices.Equal(seen.AllowedConnectors, []string{"razorpay"}) || seen.Metadata["plan"] != "pro" {
			t.Errorf("route request %+v", seen)
		}
		_, err = svc.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 500, Currency: "USD"})
		if !errors.Is(err, core.ErrNoRoute) {
			t.Fatalf("want ErrNoRoute, got %v", err)
		}
		if e.count("SELECT COUNT(*) FROM payments") != 1 {
			t.Error("a payment was created for an unroutable request")
		}
	})
}

func TestSetBillingCreatesThenUpdatesCustomer(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100) // no customer yet
		if p.CustomerID != "" {
			t.Fatal("setup")
		}
		err := e.svc.SetBilling(ctx, p.ID, core.BillingInput{Name: "Asha", Email: "asha@example.com",
			Billing: store.Billing{StateCode: "29", Country: "IN"}})
		if err != nil {
			t.Fatal(err)
		}
		got := e.get(p.ID)
		if got.CustomerID == "" {
			t.Fatal("customer not linked")
		}
		c, err := e.svc.GetCustomer(ctx, got.CustomerID)
		if err != nil || c.Name != "Asha" || c.Billing.StateCode != "29" || c.AppID != "app_1" || c.Mode != "test" {
			t.Fatalf("customer %+v %v", c, err)
		}
		err = e.svc.SetBilling(ctx, p.ID, core.BillingInput{Name: "Asha K", Billing: store.Billing{GSTIN: "29AAAAA0000A1Z5", Line1: "1 MG Road"}})
		if err != nil {
			t.Fatal(err)
		}
		c2, _ := e.svc.GetCustomer(ctx, got.CustomerID)
		if c2.ID != c.ID || c2.Name != "Asha K" || c2.Email != "asha@example.com" || c2.Billing.StateCode != "29" ||
			c2.Billing.GSTIN != "29AAAAA0000A1Z5" || c2.Billing.Line1 != "1 MG Road" {
			t.Errorf("merge failed: %+v", c2)
		}
		if err := e.svc.SetBilling(ctx, "pay_missing", core.BillingInput{Name: "x"}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing payment: %v", err)
		}
	})
}

func TestRecordCheckoutAndProviderIDsStick(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment(100)
		a := e.attempt(p.ID)
		if err := e.svc.RecordCheckout(ctx, a.ID, "order_1"); err != nil {
			t.Fatal(err)
		}
		e.apply(a.ID, core.Snapshot{Status: state.AttemptPending})
		got, _ := e.svc.GetAttempt(ctx, a.ID)
		if got.ProviderOrderID != "order_1" {
			t.Errorf("order id lost by a later snapshot: %+v", got)
		}
		e.apply(a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 100, Currency: "INR", ProviderPaymentID: "pay_1", ProviderOrderID: "order_1"})
		got, _ = e.svc.GetAttempt(ctx, a.ID)
		if got.ProviderPaymentID != "pay_1" || got.ProviderOrderID != "order_1" {
			t.Errorf("%+v", got)
		}
		if err := e.svc.RecordCheckout(ctx, "att_missing", "o"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing attempt: %v", err)
		}
	})
}

func paidPayment(t *testing.T, e *env, amount int64) (store.Payment, store.Attempt) {
	t.Helper()
	p := e.payment(amount)
	a := e.attempt(p.ID)
	e.apply(a.ID, captured(amount))
	return e.get(p.ID), a
}

func TestApplyRefundSnapshotSucceeded(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paidPayment(t, e, 1000)
		r, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 400})
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundSucceeded, ProviderRefundID: "rfnd_1"})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Changed || res.Refund.Status != "succeeded" || res.Refund.ProviderRefundID != "rfnd_1" {
			t.Fatalf("refund %+v", res)
		}
		got := e.get(p.ID)
		if got.AmountRefunded != 400 || !got.HasFlag("refunded_partial") || got.HasFlag("refunded_full") || got.Status != "succeeded" || got.Version != p.Version+1 {
			t.Errorf("payment %+v", got)
		}
		var ledger []store.LedgerEntry
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { ledger, err = store.ListLedgerEntries(ctx, q, p.ID); return })
		refundAt := slices.IndexFunc(ledger, func(entry store.LedgerEntry) bool { return entry.Kind == "refund" })
		if len(ledger) != 2 || refundAt < 0 || ledger[refundAt].Amount != -400 || ledger[refundAt].RefundID != r.ID || ledger[refundAt].Currency != "INR" {
			t.Errorf("ledger %+v", ledger)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "refund.succeeded" {
			t.Errorf("events %v", ev)
		}
		// Replay changes nothing.
		before := len(e.emitted())
		res, _ = e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundSucceeded, ProviderRefundID: "rfnd_1"})
		if res.Changed || len(e.emitted()) != before || e.get(p.ID).AmountRefunded != 400 || e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'refund'") != 1 {
			t.Error("replay applied twice")
		}
		// The rest completes the refund and flips the flag.
		r2, _ := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID})
		e.svc.ApplyRefundSnapshot(ctx, r2.ID, core.RefundSnapshot{Status: state.RefundSucceeded})
		got = e.get(p.ID)
		if got.AmountRefunded != 1000 || !got.HasFlag("refunded_full") || got.HasFlag("refunded_partial") {
			t.Errorf("full refund flags: %+v", got)
		}
		// A terminal refund cannot flip to the other terminal state.
		if _, err := e.svc.ApplyRefundSnapshot(ctx, r2.ID, core.RefundSnapshot{Status: state.RefundFailed}); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("flip: %v", err)
		}
	})
}

func TestApplyRefundSnapshotFailedFreesBalance(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paidPayment(t, e, 1000)
		in := core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID}
		r, _ := e.svc.CreateRefund(ctx, in)
		res, err := e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundFailed, FailureReason: "bank refused"})
		if err != nil || res.Refund.Status != "failed" {
			t.Fatalf("%+v %v", res, err)
		}
		if got := e.get(p.ID); got.AmountRefunded != 0 || got.HasFlag("refunded_full") {
			t.Errorf("failed refund touched the payment: %+v", got)
		}
		if e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'refund'") != 0 {
			t.Error("failed refund wrote a ledger entry")
		}
		if ev := e.emitted(); ev[len(ev)-1] != "refund.failed" {
			t.Errorf("events %v", ev)
		}
		if _, err := e.svc.CreateRefund(ctx, in); err != nil {
			t.Errorf("balance not freed after failure: %v", err)
		}
		if _, err := e.svc.ApplyRefundSnapshot(ctx, "ref_missing", core.RefundSnapshot{Status: state.RefundFailed}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
		if _, err := e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: "weird"}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("bad status: %v", err)
		}
	})
}

func TestApplyRefundSnapshotIsAtomic(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paidPayment(t, e, 1000)
		r, _ := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID})
		e.failEmit = true
		if _, err := e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundSucceeded}); err == nil {
			t.Fatal("expected emitter error")
		}
		e.failEmit = false
		got, _ := e.svc.GetRefund(ctx, e.scope, r.ID)
		if got.Status != "pending" || e.get(p.ID).AmountRefunded != 0 || e.count("SELECT COUNT(*) FROM ledger_entries WHERE kind = 'refund'") != 0 {
			t.Error("partial refund result survived")
		}
	})
}

func TestAddDomainAndLookup(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		d, err := e.svc.AddDomain(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, "app_1", "Pay.Example.org.")
		if err != nil {
			t.Fatal(err)
		}
		if d.Host != "pay.example.org" || !d.IsPrimary || d.Status != "pending" || len(d.Token) < 32 {
			t.Fatalf("domain %+v", d)
		}
		d2, err := e.svc.AddDomain(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, "app_1", "checkout.example.org")
		if err != nil || d2.IsPrimary {
			t.Errorf("second domain must not be primary: %+v %v", d2, err)
		}
		if _, err := e.svc.AddDomain(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, "app_1", "pay.example.org"); !errors.Is(err, core.ErrConflict) {
			t.Errorf("duplicate: %v", err)
		}
		for _, bad := range []string{"", "no spaces.example.org", "nodots", "-bad.example.org", "http://x.example.org", "a..b.example.org", "1.2.3.4"} {
			if _, err := e.svc.AddDomain(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, "app_1", bad); !errors.Is(err, core.ErrInvalid) {
				t.Errorf("%q: %v", bad, err)
			}
		}
		if _, err := e.svc.AddDomain(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, "app_x", "x.example.org"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing app: %v", err)
		}
		var _ domains.Lookup = e.svc
		got, ok, err := e.svc.LookupHost(ctx, "pay.example.org")
		if err != nil || !ok || got.AppID != "app_1" || got.Status != domains.StatusPending {
			t.Errorf("lookup: %+v %v %v", got, ok, err)
		}
		if _, ok, _ := e.svc.LookupHost(ctx, "unknown.example.org"); ok {
			t.Error("unknown host found")
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'domain.add'") != 2 {
			t.Error("domain.add must be audited")
		}
	})
}

var _ = events.Event{}

// otherOrgApp creates a second org with an app and returns the app id.
func otherOrgApp(t *testing.T, e *env) string {
	t.Helper()
	err := e.db.Write(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO orgs (id, name, created_at) VALUES ('org_other', 'Other', 1)"); err != nil {
			return err
		}
		return store.InsertApp(ctx, tx, store.App{ID: "app_other", OrgID: "org_other", Name: "O", Slug: "o", CreatedAt: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	return "app_other"
}

func TestAddDomainRejectsOtherOrgApp(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		other := otherOrgApp(t, e)
		mine := core.OrgScope{OrgID: store.DefaultOrgID}
		if _, err := e.svc.AddDomain(ctx, audit.System, mine, other, "pay.example.org"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("cross-org AddDomain: %v", err)
		}
		if e.count("SELECT COUNT(*) FROM app_domains") != 0 || e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'domain.add'") != 0 {
			t.Error("rejected call left rows behind")
		}
		d, err := e.svc.AddDomain(ctx, audit.System, core.OrgScope{OrgID: "org_other"}, other, "pay.example.org")
		if err != nil || d.OrgID != "org_other" {
			t.Errorf("own-org AddDomain: %+v %v", d, err)
		}
		if e.count("SELECT COUNT(*) FROM audit_log WHERE action = 'domain.add' AND org_id = 'org_other'") != 1 {
			t.Error("audit entry must use the scope's org")
		}
	})
}

func TestAPIKeyAndAppMutationsAreOrgScoped(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		other := otherOrgApp(t, e)
		mine := core.OrgScope{OrgID: store.DefaultOrgID}
		if _, _, err := e.svc.CreateAPIKey(ctx, audit.System, mine, other, "test"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("cross-org CreateAPIKey: %v", err)
		}
		if e.count("SELECT COUNT(*) FROM api_keys WHERE app_id = ?", other) != 0 {
			t.Error("key minted for another org's app")
		}
		app, err := e.svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: "org_other"}, core.CreateAppInput{Name: "N", Slug: "n"})
		if err != nil || app.OrgID != "org_other" {
			t.Errorf("CreateApp must use the scope's org: %+v %v", app, err)
		}
		if _, err := e.svc.AppBySlug(ctx, mine, "n"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("AppBySlug leaked another org's app: %v", err)
		}
		if _, err := e.svc.CreateApp(ctx, audit.System, core.OrgScope{}, core.CreateAppInput{Name: "N", Slug: "z"}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("empty scope: %v", err)
		}
	})
}
