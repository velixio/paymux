package checkout_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/disputes"
	"github.com/velixio/paymux/internal/events"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/inbound"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/routing"
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

type env struct {
	t       *testing.T
	db      *store.DB
	core    *core.Service
	conns   *connectors.Service
	flow    *checkout.Service
	set     *settings.Service
	q       *jobs.Queue
	breaker *routing.Breaker
	fake    *fakeconn.State
	clk     *clock
	scope   core.Scope
	mu      sync.Mutex
	events  []events.Event
	pa1     string
	pa2     string
	alerts  *alertLog
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
	e := &env{t: t, db: db, clk: clk, set: settings.New(db), fake: fakeconn.Register("fake"),
		breaker: routing.NewBreaker(clk.Now), alerts: &alertLog{}, scope: core.Scope{AppID: "app_1", Mode: "test"}}
	e.q = jobs.New(db, jobs.Options{Clock: clk.Now})
	box, err := secrets.NewBox(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://pay.example.com")
	e.conns = connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clk.Now})
	rt := checkout.NewRouting(e.breaker).WithSettings(e.set)
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Jobs: e.q, Router: rt, Now: clk.Now,
		Emitter: events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.events = append(e.events, ev)
			return nil
		})})
	e.flow = checkout.New(checkout.Options{DB: db, Core: e.core, Conns: e.conns, Routing: rt, Breaker: e.breaker, Alerts: e.alerts, Now: clk.Now})
	if _, err := core.Bootstrap(ctx, db, e.set); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx store.Tx) error {
		return store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "A", Slug: "a", CreatedAt: clk.t})
	}); err != nil {
		t.Fatal(err)
	}
	e.pa1, e.pa2 = e.account("first").ID, e.account("second").ID
	e.routeFake(`{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"` + e.fake.Name + `"}]}}`)
	return e
}

// account creates a linked, active account on the fake connector.
func (e *env) account(name string) store.ProviderAccount {
	pa, err := e.conns.CreateAccount(ctx, audit.System, org, connectors.CreateAccountInput{Connector: e.fake.Name, Name: name,
		Mode: "test", Config: json.RawMessage(fakeconn.ValidConfig), AppID: "app_1"})
	if err != nil {
		e.t.Fatal(err)
	}
	return pa
}

// routeFake makes the org routing send everything to the given catch-all rule.
func (e *env) routeFake(catchAll string) {
	e.t.Helper()
	doc := `{"rules":[],"catch_all":` + catchAll + `}`
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(doc)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) payment(amount int64, cur string) store.Payment {
	e.t.Helper()
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: amount, Currency: cur, OrderRef: "o1",
		Description: "Pro", Metadata: map[string]string{"plan": "pro"},
		Customer: &core.CustomerInput{ExternalRef: "u1", Name: "Asha", Email: "asha@example.com", Phone: "+919999999999"}})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Payment
}

func (e *env) payment2() store.Payment { return e.payment(50000, "INR") }

func (e *env) get(id string) store.Payment {
	e.t.Helper()
	p, err := e.core.GetPayment(ctx, e.scope, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(ctx, func(q store.Querier) error { return q.QueryRowContext(ctx, query, args...).Scan(&n) }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) attempts(paymentID string) []store.Attempt {
	e.t.Helper()
	var out []store.Attempt
	_ = e.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.ListAttempts(ctx, q, paymentID); return })
	return out
}

func (e *env) setPayment(ref string, s connector.PaymentSnapshot) {
	e.fake.Locked(func() { e.fake.Payments[ref] = s })
}

func each(t *testing.T, fn func(t *testing.T, e *env)) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) { fn(t, newEnv(t, db)) })
}

func succeeded(amount int64, cur, payID string) connector.PaymentSnapshot {
	return connector.PaymentSnapshot{Status: connector.PaymentSucceeded, RawStatus: "captured", Amount: amount, Currency: cur, ProviderPaymentID: payID, Method: "upi"}
}

// ---- routing ----

func TestRoutingUsesSeededDefaultsAndAppOverrides(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// Restore the spec defaults: INR goes razorpay, zoho, payu, and nothing of
		// that is linked to this app, so there is no route.
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", json.RawMessage(mustDefault(t))); err != nil {
			t.Fatal(err)
		}
		_, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR"})
		if !errors.Is(err, core.ErrNoRoute) {
			t.Fatalf("default rules with unlinked connectors: %v", err)
		}
		// An app override is evaluated first and can route to a connector by name.
		app := `{"rules":[{"name":"mine","when":{"currency":["INR"]},"action":{"kind":"route","targets":[{"connector":"` + e.fake.Name + `"}]}}]}`
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "routing", json.RawMessage(app)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR"}); err != nil {
			t.Fatalf("app rule: %v", err)
		}
		// The app override carries no catch-all, so other currencies fall to the org catch-all (razorpay/whop: unlinked).
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "USD"}); !errors.Is(err, core.ErrNoRoute) {
			t.Fatalf("USD: %v", err)
		}
	})
}

func mustDefault(t *testing.T) string {
	d, ok := settings.Lookup("routing")
	if !ok {
		t.Fatal("no routing setting")
	}
	return d.Default
}

func TestRoutingDefaultRulesMatchSpec(t *testing.T) {
	var cfg struct {
		Rules    []routing.Rule `json:"rules"`
		CatchAll routing.Rule   `json:"catch_all"`
	}
	d, _ := settings.Lookup("routing")
	if err := json.Unmarshal([]byte(d.Default), &cfg); err != nil {
		t.Fatal(err)
	}
	connectorsOf := func(r routing.Rule) []string {
		var out []string
		for _, tg := range r.Action.Targets {
			out = append(out, tg.Connector)
		}
		return out
	}
	rc := routing.Config{Org: cfg.Rules, CatchAll: cfg.CatchAll}
	accounts := []routing.Account{
		{ID: "r", Connector: "razorpay", Currencies: []string{"INR", "BDT", "USD"}, Purposes: []string{"one_time"}},
		{ID: "z", Connector: "zoho", Currencies: []string{"INR"}, Purposes: []string{"one_time"}},
		{ID: "p", Connector: "payu", Currencies: []string{"INR"}, Purposes: []string{"one_time"}},
		{ID: "w", Connector: "whop", Currencies: []string{"BDT", "USD"}, Purposes: []string{"one_time"}},
	}
	for cur, want := range map[string][]string{"INR": {"r", "z", "p"}, "BDT": {"r", "w"}, "USD": {"r", "w"}} {
		got, err := routing.Choose(rc, routing.Request{Currency: cur, Purpose: "one_time"}, accounts, nil)
		if err != nil || !slices.Equal(got.Accounts, want) {
			t.Errorf("%s: %v %v want %v", cur, got.Accounts, err, want)
		}
	}
	if len(connectorsOf(cfg.CatchAll)) != 2 {
		t.Error("catch-all")
	}
}

func TestCheckRouteRejectsDisabledAndUnacceptedCurrencies(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.routeFake(`{"name":"no-usd","when":{"currency":["USD"]},"action":{"kind":"reject","message":"USD is not accepted here"}}`)
		_, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "USD"})
		var ce *core.Error
		if !errors.Is(err, core.ErrCurrencyNotAccepted) || !errors.As(err, &ce) || ce.Message != "USD is not accepted here" {
			t.Fatalf("reject rule: %v", err)
		}
		e.routeFake(`{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"` + e.fake.Name + `"}]}}`)
		// BDT is declared by the connector, so narrow both accounts to INR.
		for _, id := range []string{e.pa1, e.pa2} {
			if err := e.conns.UpdateAccount(ctx, audit.System, org, id, connectors.UpdateAccountInput{Currencies: []string{"INR"}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "BDT"}); !errors.Is(err, core.ErrNoRoute) {
			t.Errorf("currency narrowed away: %v", err)
		}
		disabled := "disabled"
		_ = e.conns.UpdateAccount(ctx, audit.System, org, e.pa1, connectors.UpdateAccountInput{Status: &disabled})
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR"}); err != nil {
			t.Errorf("one disabled account must not block routing: %v", err)
		}
		_ = e.conns.UpdateAccount(ctx, audit.System, org, e.pa2, connectors.UpdateAccountInput{Status: &disabled})
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR"}); !errors.Is(err, core.ErrNoRoute) {
			t.Errorf("all accounts disabled: %v", err)
		}
		// Allowed connectors restrict the choice.
		active := "active"
		_ = e.conns.UpdateAccount(ctx, audit.System, org, e.pa1, connectors.UpdateAccountInput{Status: &active})
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: 100, Currency: "INR", AllowedConnectors: []string{"razorpay"}}); !errors.Is(err, core.ErrNoRoute) {
			t.Errorf("allowed_connectors: %v", err)
		}
	})
}

func TestRoutingIsPerMode(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		live := core.Scope{AppID: "app_1", Mode: "live"} // accounts are test-mode only
		if _, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: live, Amount: 100, Currency: "INR"}); !errors.Is(err, core.ErrNoRoute) {
			t.Errorf("live payment routed to test accounts: %v", err)
		}
	})
}

// ---- starting a checkout ----

func TestStartCheckoutRedirect(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment2()
		st, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.Action.Kind != connector.ActionRedirect || st.Connector != e.fake.Name || st.AccountID != e.pa1 && st.AccountID != e.pa2 {
			t.Fatalf("started %+v", st)
		}
		var req connector.CheckoutRequest
		e.fake.Locked(func() { req = e.fake.Checkouts[0] })
		if req.PaymentID != p.ID || req.AttemptID != st.Attempt.ID || req.ProviderRef != st.Attempt.ProviderRef || req.Amount != 50000 ||
			req.Currency != "INR" || req.AppID != "app_1" || req.Description != "Pro" || req.Metadata["plan"] != "pro" ||
			req.Customer.Email != "asha@example.com" || req.Customer.Name != "Asha" || req.Customer.Phone != "+919999999999" {
			t.Errorf("checkout request %+v", req)
		}
		a := e.attempts(p.ID)
		if len(a) != 1 || a[0].ProviderOrderID != "order_"+a[0].ProviderRef || a[0].Status != "created" {
			t.Errorf("attempts %+v", a)
		}
		if got := e.get(p.ID); got.Status != "created" {
			t.Errorf("payment moved before the customer acted: %s", got.Status)
		}
	})
}

func TestStartCheckoutFallsBackToNextAccount(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment2()
		calls := 0
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(req connector.CheckoutRequest) (connector.CheckoutResult, error) {
				calls++
				if calls == 1 {
					return connector.CheckoutResult{}, connector.ErrTemporary
				}
				return connector.CheckoutResult{ProviderOrderID: "o2", NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: "https://x.example.com/p"}}, nil
			}
		})
		st, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		a := e.attempts(p.ID) // ids within one millisecond are unordered: tell the attempts apart by status
		if len(a) != 2 {
			t.Fatalf("attempts %+v", a)
		}
		if a[0].Status != "failed" {
			a[0], a[1] = a[1], a[0]
		}
		if a[0].Status != "failed" || a[0].FailureCode != "checkout_error" || a[1].Status != "created" || a[1].ID != st.Attempt.ID {
			t.Fatalf("attempts %+v", a)
		}
		if a[0].ProviderAccountID == a[1].ProviderAccountID {
			t.Error("fallback must use the next account")
		}
		if got := e.get(p.ID); got.Status != "created" {
			t.Errorf("a failed first checkout must not fail the payment: %s", got.Status)
		}
		if slices.Contains(e.emitted(), "payment.failed") {
			t.Error("payment.failed emitted during fallback")
		}
	})
}

func TestStartCheckoutAllFail(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment2()
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{}, errors.New("provider down")
			}
		})
		_, err := e.flow.StartCheckout(ctx, p.ID)
		if !errors.Is(err, checkout.ErrCheckoutFailed) {
			t.Fatalf("want ErrCheckoutFailed, got %v", err)
		}
		a := e.attempts(p.ID)
		if len(a) != 2 || a[0].Status != "failed" || a[1].Status != "failed" {
			t.Errorf("attempts %+v", a)
		}
		if got := e.get(p.ID); got.Status != "failed" {
			t.Errorf("payment should fail once every account failed: %s", got.Status)
		}
	})
}

func TestBreakerRecordsFailuresAndSuccessesAndReordersAccounts(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// The first account tried for each payment always errors and the next
		// one works, so the first-choice account collects the failures.
		calls := 0
		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(req connector.CheckoutRequest) (connector.CheckoutResult, error) {
				calls++
				if calls%2 == 1 {
					return connector.CheckoutResult{}, errors.New("down")
				}
				return connector.CheckoutResult{ProviderOrderID: "o", NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: "https://x.example.com"}}, nil
			}
		})
		first := ""
		for i := 0; i < routing.BreakerFailures; i++ {
			if first != "" && e.breaker.Open(first) {
				t.Fatalf("breaker opened after only %d failures", i)
			}
			st, err := e.flow.StartCheckout(ctx, e.payment2().ID)
			if err != nil {
				t.Fatal(err)
			}
			if first == "" {
				for _, at := range e.attempts(st.Attempt.PaymentID) {
					if at.Status == "failed" {
						first = at.ProviderAccountID
					}
				}
			}
			if st.AccountID == first {
				t.Fatal("the failing first choice must not be the one that succeeded")
			}
		}
		if !e.breaker.Open(first) {
			t.Fatal("breaker did not open after repeated failures")
		}
		if e.breaker.Open(e.otherAccount(first)) {
			t.Error("the working account's breaker must stay closed")
		}
		// With the breaker open the account is tried last, so the first call succeeds.
		e.fake.Locked(func() { e.fake.CheckoutFn = nil })
		st, err := e.flow.StartCheckout(ctx, e.payment2().ID)
		if err != nil || st.AccountID == first {
			t.Errorf("open breaker must push the account last: used %s (open: %s) err=%v", st.AccountID, first, err)
		}
		// After the cooldown it is tried first again.
		e.clk.Add(routing.BreakerCooldown + time.Second)
		if e.breaker.Open(first) {
			t.Error("breaker should close after the cooldown")
		}
		st, err = e.flow.StartCheckout(ctx, e.payment2().ID)
		if err != nil || st.AccountID != first {
			t.Errorf("recovered account should be first choice again: used %s want %s err=%v", st.AccountID, first, err)
		}
	})
}

func (e *env) otherAccount(id string) string {
	if id == e.pa1 {
		return e.pa2
	}
	return e.pa1
}

func TestStartCheckoutRefusesFinishedPayments(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p := e.payment2()
		if _, err := e.core.CancelPayment(ctx, e.scope, audit.System, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.flow.StartCheckout(ctx, p.ID); !errors.Is(err, core.ErrInvalidState) {
			t.Errorf("canceled: %v", err)
		}
		if _, err := e.flow.StartCheckout(ctx, "pay_missing"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("missing: %v", err)
		}
	})
}

// ---- return ----

func startedAttempt(t *testing.T, e *env) (store.Payment, checkout.Started) {
	t.Helper()
	p := e.payment2()
	st, err := e.flow.StartCheckout(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, st
}

func TestHandleReturnFetchesTruthAndApplies(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		e.setPayment(ref, succeeded(50000, "INR", "pay_9"))
		got, err := e.flow.HandleReturn(ctx, st.AccountID, ref, checkout.ReturnInput{Method: "POST", Form: map[string][]string{"sig": {"ok"}, "payment_id": {"pay_9"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != p.ID || got.Status != "succeeded" || got.AmountReceived != 50000 {
			t.Fatalf("payment %+v", got)
		}
		var fetched connector.PaymentRef
		e.fake.Locked(func() { fetched = e.fake.Fetches[0] })
		if fetched.ProviderRef != ref || fetched.ProviderOrderID != st.Attempt.ProviderOrderID || fetched.ProviderPaymentID != "pay_9" {
			t.Errorf("fetch ref %+v", fetched)
		}
		a := e.attempts(p.ID)[0]
		if a.ProviderPaymentID != "pay_9" || a.Status != "captured" {
			t.Errorf("attempt %+v", a)
		}
	})
}

func TestHandleReturnWithBadSignatureStillUsesOnlyTheFetchedTruth(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		// Nothing was paid at the provider: a forged "success" return changes nothing.
		got, err := e.flow.HandleReturn(ctx, st.AccountID, st.Attempt.ProviderRef,
			checkout.ReturnInput{Method: "GET", Query: map[string][]string{"status": {"success"}, "payment_id": {"pay_forged"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != p.ID || got.Status != "created" {
			t.Errorf("forged return moved the payment: %+v", got)
		}
		// The forged payment id from an unverified return must not be fetched or stored.
		e.fake.Locked(func() {
			for _, f := range e.fake.Fetches {
				if f.ProviderPaymentID == "pay_forged" {
					t.Error("unverified payment id was used")
				}
			}
		})
		if a := e.attempts(p.ID)[0]; a.ProviderPaymentID != "" {
			t.Errorf("attempt %+v", a)
		}
	})
}

func TestHandleReturnScopedToAccountAndRef(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, st := startedAttempt(t, e)
		other := e.pa1
		if st.AccountID == e.pa1 {
			other = e.pa2
		}
		if _, err := e.flow.HandleReturn(ctx, other, st.Attempt.ProviderRef, checkout.ReturnInput{Method: "GET"}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("wrong account: %v", err)
		}
		if _, err := e.flow.HandleReturn(ctx, st.AccountID, "NOPE", checkout.ReturnInput{Method: "GET"}); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("unknown ref: %v", err)
		}
	})
}

// ---- inbound events ----

func storeEvent(t *testing.T, e *env, account string, verified bool, body any) string {
	t.Helper()
	raw, _ := json.Marshal(body)
	id := "pev_" + time.Now().Format("150405.000000")
	row := store.ProviderEventRow{ID: id, ProviderAccountID: account, ProviderEventID: id, Headers: json.RawMessage(`{"X-Fake-Sig":["ok"]}`),
		Body: raw, Verified: verified, ReceivedAt: e.clk.Now()}
	if err := e.db.Write(ctx, func(tx store.Tx) error { _, err := e.db.InsertProviderEvent(ctx, tx, row); return err }); err != nil {
		t.Fatal(err)
	}
	return id
}

func jobPayload(account, event string) json.RawMessage {
	b, _ := json.Marshal(inbound.JobPayload{ProviderEventID: event, ProviderAccountID: account})
	return b
}

func TestProcessProviderEventAppliesFetchedTruth(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		// The webhook claims failure; the API says paid. The API wins.
		e.setPayment(ref, succeeded(50000, "INR", "pay_7"))
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "e1", EventType: "payment.failed",
			Payment: &connector.PaymentSnapshot{ProviderRef: ref, Status: connector.PaymentFailed, Amount: 50000, Currency: "INR"}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status != "succeeded" {
			t.Fatalf("payment %s", got.Status)
		}
		pe, _ := e.db.GetProviderEvent(ctx, st.AccountID, ev)
		if pe.ProcessedAt.IsZero() || pe.Error != "" {
			t.Errorf("event not marked processed: %+v", pe)
		}
		// Replaying the job changes nothing.
		n := len(e.emitted())
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil || len(e.emitted()) != n {
			t.Errorf("replay: %v events %d->%d", err, n, len(e.emitted()))
		}
	})
}

func TestProcessProviderEventUnsignedHintOnlyTriggersFetch(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		// Unsigned and claims success, but the provider says nothing is paid.
		ev := storeEvent(t, e, st.AccountID, false, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "u1",
			Payment: &connector.PaymentSnapshot{ProviderRef: st.Attempt.ProviderRef, Status: connector.PaymentSucceeded, Amount: 50000, Currency: "INR"}}})
		e.setPayment(st.Attempt.ProviderRef, connector.PaymentSnapshot{Status: connector.PaymentRequiresAction, RawStatus: "created", Amount: 50000, Currency: "INR"})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status == "succeeded" {
			t.Fatal("an unsigned webhook changed state without the API confirming it")
		}
		e.fake.Locked(func() {
			if len(e.fake.Fetches) == 0 {
				t.Error("the hint must still trigger a fetch")
			}
		})
	})
}

func TestProcessProviderEventAppliesSuccessfulRetryOnSameOrder(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		orderID := st.Attempt.ProviderOrderID
		failed := connector.PaymentSnapshot{ProviderRef: st.Attempt.ProviderRef, ProviderOrderID: orderID,
			ProviderPaymentID: "pay_failed", Status: connector.PaymentFailed, RawStatus: "failed", Amount: 50000, Currency: "INR"}
		e.setPayment(st.Attempt.ProviderRef, failed)
		if _, err := e.flow.SyncAttempt(ctx, st.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		failedPayment, failedAttempt := e.get(p.ID), e.attempts(p.ID)[0]
		if failedPayment.Status != "failed" || failedAttempt.Status != "failed" || failedAttempt.ProviderPaymentID != "pay_failed" {
			t.Fatalf("first provider payment was not applied: payment=%+v attempt=%+v", failedPayment, failedAttempt)
		}
		if got := e.get(p.ID); got.Status != "failed" {
			t.Fatalf("first provider payment: status %s, want failed", got.Status)
		}

		succeededRetry := succeeded(50000, "INR", "pay_retry")
		succeededRetry.ProviderRef = st.Attempt.ProviderRef
		succeededRetry.ProviderOrderID = orderID
		e.fake.Locked(func() {
			e.fake.Payments = map[string]connector.PaymentSnapshot{"retry": succeededRetry}
		})
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "retry",
			Payment: &connector.PaymentSnapshot{ProviderOrderID: orderID, ProviderPaymentID: "pay_retry", Status: connector.PaymentSucceeded}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}

		got := e.get(p.ID)
		attempt := e.attempts(p.ID)[0]
		if got.Status != "succeeded" || got.AmountReceived != 50000 || attempt.Status != "captured" || attempt.ProviderPaymentID != "pay_retry" {
			t.Fatalf("successful retry was not applied: payment=%+v attempt=%+v", got, attempt)
		}
		e.fake.Locked(func() {
			last := e.fake.Fetches[len(e.fake.Fetches)-1]
			if last.ProviderPaymentID != "pay_retry" {
				t.Errorf("fetched provider payment %q, want retry", last.ProviderPaymentID)
			}
		})
	})
}

func TestSyncAttemptLatestSkipsAttemptWithoutProviderIdentifier(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		if err := e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE payment_attempts SET provider_ref = '', provider_order_id = NULL,
				provider_payment_id = NULL WHERE id = ?`, st.Attempt.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		before := e.fetches()
		res, err := e.flow.SyncAttemptLatest(ctx, st.Attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if res.Payment.ID != p.ID || e.fetches() != before {
			t.Fatalf("empty provider reference fetched: result=%+v fetches=%d want %d", res, e.fetches(), before)
		}
	})
}

func TestSyncAttemptLatestFindsSuccessfulRetryOnSameOrder(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		orderID := st.Attempt.ProviderOrderID
		failed := connector.PaymentSnapshot{ProviderRef: st.Attempt.ProviderRef, ProviderOrderID: orderID,
			ProviderPaymentID: "pay_failed", Status: connector.PaymentFailed, RawStatus: "failed", Amount: 50000, Currency: "INR"}
		e.setPayment(st.Attempt.ProviderRef, failed)
		if _, err := e.flow.SyncAttempt(ctx, st.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		failedPayment, failedAttempt := e.get(p.ID), e.attempts(p.ID)[0]
		if failedPayment.Status != "failed" || failedAttempt.Status != "failed" || failedAttempt.ProviderPaymentID != "pay_failed" {
			t.Fatalf("first provider payment was not applied: payment=%+v attempt=%+v", failedPayment, failedAttempt)
		}

		succeededRetry := succeeded(50000, "INR", "pay_retry")
		succeededRetry.ProviderRef = st.Attempt.ProviderRef
		succeededRetry.ProviderOrderID = orderID
		e.fake.Locked(func() {
			e.fake.Payments = map[string]connector.PaymentSnapshot{"retry": succeededRetry}
		})
		if _, err := e.flow.SyncAttemptLatest(ctx, st.Attempt.ID); err != nil {
			t.Fatal(err)
		}

		got := e.get(p.ID)
		attempt := e.attempts(p.ID)[0]
		if got.Status != "succeeded" || attempt.Status != "captured" || attempt.ProviderPaymentID != "pay_retry" {
			t.Fatalf("successful retry was not found: payment=%+v attempt=%+v", got, attempt)
		}
		var providerAttempts []store.ProviderPaymentAttempt
		if err := e.db.Read(ctx, func(q store.Querier) (err error) {
			providerAttempts, err = store.ListProviderPaymentAttempts(ctx, q, p.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(providerAttempts) != 2 || providerAttempts[0].ProviderPaymentID != "pay_failed" ||
			providerAttempts[1].ProviderPaymentID != "pay_retry" {
			t.Fatalf("provider payment attempts = %+v, want failed then retry", providerAttempts)
		}
		e.fake.Locked(func() {
			last := e.fake.Fetches[len(e.fake.Fetches)-1]
			if last.ProviderPaymentID != "" || last.ProviderOrderID != orderID {
				t.Errorf("latest sync fetched %+v, want order without stale payment id", last)
			}
		})
	})
}

func TestProcessProviderEventUnknownObjectsAreRecorded(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, st := startedAttempt(t, e)
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{
			{Kind: connector.NotifyPayment, EventID: "e2", Payment: &connector.PaymentSnapshot{ProviderPaymentID: "pay_stranger", Status: connector.PaymentSucceeded}},
			{Kind: connector.NotifyRefund, EventID: "e2", Refund: &connector.RefundSnapshot{ProviderRefundID: "rfnd_stranger", Status: connector.RefundSucceeded}},
			{Kind: connector.NotifyDispute, EventID: "e2", Dispute: &connector.DisputeSnapshot{ProviderDisputeID: "dsp_1", ProviderPaymentID: "pay_stranger"}},
		})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		var objs []store.UnknownObject
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { objs, err = store.ListUnknownObjects(ctx, q, st.AccountID); return })
		kinds := map[string]string{}
		for _, o := range objs {
			kinds[o.Kind] = o.ProviderID
		}
		if kinds["payment"] != "pay_stranger" || kinds["refund"] != "rfnd_stranger" || kinds["dispute"] != "dsp_1" {
			t.Errorf("unknown objects %+v", objs)
		}
	})
}

func TestProcessProviderEventNeverTouchesAnotherAccountsObjects(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		other := e.pa1
		if st.AccountID == e.pa1 {
			other = e.pa2
		}
		e.setPayment(st.Attempt.ProviderRef, succeeded(50000, "INR", "pay_1"))
		// A validly signed event of the OTHER account naming our attempt's reference.
		ev := storeEvent(t, e, other, true, []connector.Notification{{Kind: connector.NotifyPayment, EventID: "x",
			Payment: &connector.PaymentSnapshot{ProviderRef: st.Attempt.ProviderRef, ProviderOrderID: st.Attempt.ProviderOrderID, Status: connector.PaymentSucceeded}}})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(other, ev)); err != nil {
			t.Fatal(err)
		}
		if got := e.get(p.ID); got.Status != "created" {
			t.Fatalf("cross-account event moved the payment: %s", got.Status)
		}
		if e.count("SELECT COUNT(*) FROM unknown_provider_objects WHERE provider_account_id = ?", other) != 1 {
			t.Error("cross-account reference should be recorded as unknown for the delivering account")
		}
	})
}

func TestProcessProviderEventRefundNotification(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		e.setPayment(st.Attempt.ProviderRef, succeeded(50000, "INR", "pay_1"))
		if _, err := e.flow.SyncAttempt(ctx, st.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 20000})
		if err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			e.fake.Refunds[r.ProviderRef] = connector.RefundSnapshot{ProviderRefundID: "rfnd_5", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, RawStatus: "processed", Amount: 20000, Currency: "INR"}
		})
		ev := storeEvent(t, e, st.AccountID, true, []connector.Notification{{Kind: connector.NotifyRefund, EventID: "r1",
			Refund: &connector.RefundSnapshot{ProviderRefundID: "rfnd_5", Status: connector.RefundSucceeded}}})
		// Not yet known by provider id: it is matched by the receipt after the first sync? No receipt here,
		// so the refund must be found through the provider_refund_id stored at submit time.
		_ = e.db.Write(ctx, func(tx store.Tx) error {
			r.ProviderRefundID = "rfnd_5"
			return store.UpdateRefund(ctx, tx, r, e.clk.Now())
		})
		if err := e.flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		got, _ := e.core.GetRefund(ctx, e.scope, r.ID)
		if got.Status != "succeeded" || e.get(p.ID).AmountRefunded != 20000 {
			t.Fatalf("refund %+v payment %+v", got, e.get(p.ID))
		}
		if ev := e.emitted(); ev[len(ev)-1] != "refund.succeeded" {
			t.Errorf("events %v", ev)
		}
	})
}

// ---- refund submission ----

func paid(t *testing.T, e *env) (store.Payment, checkout.Started) {
	p, st := startedAttempt(t, e)
	e.setPayment(st.Attempt.ProviderRef, succeeded(50000, "INR", "pay_1"))
	if _, err := e.flow.SyncAttempt(ctx, st.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	return p, st
}

func submitPayload(id string) json.RawMessage { return json.RawMessage(`{"refund_id":"` + id + `"}`) }

func TestSubmitRefundSuccessAndPending(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paid(t, e)
		r, _ := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 10000, Reason: "oops"})
		if err := e.flow.SubmitRefund(ctx, submitPayload(r.ID)); err != nil {
			t.Fatal(err)
		}
		var call connector.RefundRequest
		e.fake.Locked(func() { call = e.fake.RefundCalls[0] })
		if call.ProviderRef != r.ProviderRef || call.ProviderPaymentID != "pay_1" || call.Amount != 10000 || call.Currency != "INR" || call.Reason != "oops" {
			t.Errorf("refund request %+v", call)
		}
		got, _ := e.core.GetRefund(ctx, e.scope, r.ID)
		if got.Status != "pending" || got.ProviderRefundID != "rfnd_"+r.ProviderRef {
			t.Errorf("pending refund must record the provider id: %+v", got)
		}
		// A repeat of the job re-sends with the same idempotency token.
		if err := e.flow.SubmitRefund(ctx, submitPayload(r.ID)); err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			if len(e.fake.RefundCalls) != 2 || e.fake.RefundCalls[1].ProviderRef != r.ProviderRef {
				t.Errorf("calls %+v", e.fake.RefundCalls)
			}
		})
		// An immediately-final provider answer completes the refund.
		r2, _ := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 5000})
		e.fake.Locked(func() {
			e.fake.RefundFn = func(req connector.RefundRequest) (connector.RefundSnapshot, error) {
				return connector.RefundSnapshot{ProviderRefundID: "rfnd_done", Status: connector.RefundSucceeded, Amount: req.Amount, Currency: req.Currency}, nil
			}
		})
		if err := e.flow.SubmitRefund(ctx, submitPayload(r2.ID)); err != nil {
			t.Fatal(err)
		}
		got2, _ := e.core.GetRefund(ctx, e.scope, r2.ID)
		if got2.Status != "succeeded" || e.get(p.ID).AmountRefunded != 5000 {
			t.Errorf("%+v / %+v", got2, e.get(p.ID))
		}
		// Finished refunds are never re-sent.
		e.fake.Locked(func() { e.fake.RefundCalls = nil })
		if err := e.flow.SubmitRefund(ctx, submitPayload(r2.ID)); err != nil {
			t.Fatal(err)
		}
		e.fake.Locked(func() {
			if len(e.fake.RefundCalls) != 0 {
				t.Error("finished refund was sent again")
			}
		})
	})
}

func TestSubmitRefundNotRefundableFailsTheRefund(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paid(t, e)
		r, _ := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID})
		e.fake.Locked(func() {
			e.fake.RefundFn = func(connector.RefundRequest) (connector.RefundSnapshot, error) {
				return connector.RefundSnapshot{}, connector.ErrNotRefundable{Reason: "already refunded"}
			}
		})
		if err := e.flow.SubmitRefund(ctx, submitPayload(r.ID)); err != nil {
			t.Fatalf("a refusal is final, not a retryable error: %v", err)
		}
		got, _ := e.core.GetRefund(ctx, e.scope, r.ID)
		if got.Status != "failed" {
			t.Fatalf("refund %+v", got)
		}
		if ev := e.emitted(); ev[len(ev)-1] != "refund.failed" {
			t.Errorf("events %v", ev)
		}
		last := e.lastEventData()
		if last["failure_reason"] != "already refunded" {
			t.Errorf("failure reason not carried: %v", last)
		}
	})
}

func (e *env) lastEventData() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	raw, _ := json.Marshal(e.events[len(e.events)-1].Data)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func TestSubmitRefundTemporaryErrorsRetry(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paid(t, e)
		r, _ := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID})
		e.fake.Locked(func() {
			e.fake.RefundFn = func(connector.RefundRequest) (connector.RefundSnapshot, error) {
				return connector.RefundSnapshot{}, connector.ErrTemporary
			}
		})
		if err := e.flow.SubmitRefund(ctx, submitPayload(r.ID)); !errors.Is(err, connector.ErrTemporary) {
			t.Fatalf("temporary error must be returned for retry: %v", err)
		}
		if got, _ := e.core.GetRefund(ctx, e.scope, r.ID); got.Status != "pending" {
			t.Errorf("refund %+v", got)
		}
		if err := e.flow.SubmitRefund(ctx, json.RawMessage(`{"refund_id":"ref_missing"}`)); err == nil || errors.Is(err, connector.ErrTemporary) {
			t.Errorf("missing refund should be a permanent error: %v", err)
		}
		if err := e.flow.SubmitRefund(ctx, json.RawMessage(`nonsense`)); err == nil {
			t.Error("bad payload accepted")
		}
	})
}

// ---- poller ----

func TestPollDelayLadder(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}
	for i, w := range want {
		if got := checkout.PollDelay(i); got != w {
			t.Errorf("PollDelay(%d) = %v want %v", i, got, w)
		}
	}
	if checkout.PollDelay(50) != 24*time.Hour || checkout.PollDelay(-1) != time.Minute {
		t.Error("ladder clamps")
	}
	if checkout.PollWindow != 72*time.Hour {
		t.Error("poll window")
	}
}

func TestPollDuePollsOnTheLadder(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		polls := func() int {
			e.fake.Locked(func() {})
			n := 0
			e.fake.Locked(func() { n = len(e.fake.Fetches) })
			return n
		}
		if err := e.flow.PollDue(ctx); err != nil || polls() != 0 {
			t.Fatalf("nothing due yet: %v %d", err, polls())
		}
		e.clk.Add(core.FirstPollDelay + time.Second) // the first poll is scheduled with the checkout
		e.setPayment(ref, connector.PaymentSnapshot{Status: connector.PaymentRequiresAction, RawStatus: "created", Amount: 50000, Currency: "INR"})
		if err := e.flow.PollDue(ctx); err != nil || polls() != 1 {
			t.Fatalf("first poll: %v %d", err, polls())
		}
		if got := e.get(p.ID); got.Status != "requires_action" {
			t.Errorf("poll did not apply the snapshot: %s", got.Status)
		}
		// Second poll waits 2 minutes.
		e.clk.Add(time.Minute)
		_ = e.flow.PollDue(ctx)
		if polls() != 1 {
			t.Error("polled again too early")
		}
		e.clk.Add(61 * time.Second)
		_ = e.flow.PollDue(ctx)
		if polls() != 2 {
			t.Errorf("second poll missing: %d", polls())
		}
		// Payment completes at the provider; the next poll picks it up.
		e.setPayment(ref, succeeded(50000, "INR", "pay_3"))
		e.clk.Add(5*time.Minute + time.Second)
		_ = e.flow.PollDue(ctx)
		if got := e.get(p.ID); got.Status != "succeeded" {
			t.Errorf("poll missed the success: %s", got.Status)
		}
		// Terminal attempts are never polled again.
		before := polls()
		e.clk.Add(48 * time.Hour)
		_ = e.flow.PollDue(ctx)
		if polls() != before {
			t.Error("captured attempt polled")
		}
	})
}

func TestPollDueFindsSuccessfulRetryOnSameProviderOrder(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		orderID := st.Attempt.ProviderOrderID
		failed := connector.PaymentSnapshot{ProviderRef: st.Attempt.ProviderRef, ProviderOrderID: orderID,
			ProviderPaymentID: "pay_failed", Status: connector.PaymentFailed, RawStatus: "failed", Amount: 50000, Currency: "INR"}
		e.setPayment(st.Attempt.ProviderRef, failed)
		if _, err := e.flow.SyncAttempt(ctx, st.Attempt.ID); err != nil {
			t.Fatal(err)
		}

		retry := succeeded(50000, "INR", "pay_retry")
		retry.ProviderRef, retry.ProviderOrderID = st.Attempt.ProviderRef, orderID
		e.fake.Locked(func() { e.fake.Payments = map[string]connector.PaymentSnapshot{"retry": retry} })
		if err := e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE payment_attempts SET status = 'pending', next_poll_at = ? WHERE id = ?", e.clk.Now().Add(-time.Second).UnixMilli(), st.Attempt.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		if err := e.flow.PollDue(ctx); err != nil {
			t.Fatal(err)
		}
		got, attempt := e.get(p.ID), e.attempts(p.ID)[0]
		if got.Status != "succeeded" || attempt.ProviderPaymentID != "pay_retry" {
			t.Fatalf("poll did not find retry: payment=%+v attempt=%+v", got, attempt)
		}
	})
}

func TestPollDueStopsAfter72Hours(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, st := startedAttempt(t, e)
		e.setPayment(st.Attempt.ProviderRef, connector.PaymentSnapshot{Status: connector.PaymentRequiresAction, Amount: 50000, Currency: "INR"})
		e.clk.Add(73 * time.Hour)
		_ = e.flow.PollDue(ctx)
		e.fake.Locked(func() {
			if len(e.fake.Fetches) != 0 {
				t.Error("polled an attempt older than 72h")
			}
		})
	})
}

func TestPollDueSurvivesProviderErrorsAndBacksOff(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		_, st := startedAttempt(t, e)
		e.fake.Locked(func() { e.fake.FetchErr = connector.ErrTemporary })
		e.clk.Add(2 * time.Minute)
		if err := e.flow.PollDue(ctx); err != nil {
			t.Fatalf("a provider error must not fail the poll job: %v", err)
		}
		e.fake.Locked(func() { e.fake.FetchErr = nil })
		n := 0
		e.fake.Locked(func() { n = len(e.fake.Fetches) })
		_ = e.flow.PollDue(ctx) // still inside the 2m back-off window
		e.fake.Locked(func() {
			if len(e.fake.Fetches) != n {
				t.Error("failed poll was retried immediately instead of following the ladder")
			}
		})
		_ = st
	})
}

func TestPollDuePollsPendingRefunds(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paid(t, e)
		r, _ := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 100})
		e.fake.Locked(func() {
			e.fake.Refunds[r.ProviderRef] = connector.RefundSnapshot{ProviderRefundID: "rfnd_p", Status: connector.RefundSucceeded, Amount: 100, Currency: "INR"}
		})
		e.clk.Add(2 * time.Minute)
		if err := e.flow.PollDue(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := e.core.GetRefund(ctx, e.scope, r.ID); got.Status != "succeeded" || got.ProviderRefundID != "rfnd_p" {
			t.Errorf("refund %+v", got)
		}
	})
}

func TestRegisterInstallsHandlersAndSchedule(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.flow.Register(e.q); err != nil {
			t.Fatal(err)
		}
		p, _ := paid(t, e)
		if _, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID}); err != nil {
			t.Fatal(err)
		}
		e.clk.Add(2 * time.Minute)
		for i := 0; i < 3; i++ {
			if _, err := e.q.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			e.clk.Add(2 * time.Minute)
		}
		e.fake.Locked(func() {
			if len(e.fake.RefundCalls) != 1 {
				t.Errorf("refund.submit job did not reach the connector: %d", len(e.fake.RefundCalls))
			}
		})
		if e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'poll.due'") == 0 {
			t.Error("poll schedule not installed")
		}
		if e.count("SELECT COUNT(*) FROM jobs WHERE state = 'dead'") != 0 {
			t.Error("a handler is missing")
		}
	})
}

// ---- forged webhooks ----

func (e *env) hook(account, eventID, sigHeader string, notes any) (int, *httptest.ResponseRecorder) {
	e.t.Helper()
	h := inbound.New(inbound.Options{DB: e.db, Queue: e.q, Verifiers: e.conns.Verifiers(), Now: e.clk.Now})
	body, _ := json.Marshal(notes)
	req := httptest.NewRequest("POST", "/hooks/"+account, bytes.NewReader(body))
	req.SetPathValue("provider_account_id", account)
	if sigHeader != "" {
		req.Header.Set("X-Fake-Sig", sigHeader)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec
}

func TestForgedWebhookGets401AndNoFetch(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := startedAttempt(t, e)
		ref := st.Attempt.ProviderRef
		e.setPayment(ref, succeeded(50000, "INR", "pay_1")) // the provider really has the money
		notes := []connector.Notification{{Kind: connector.NotifyPayment, EventID: "evt_forged",
			Payment: &connector.PaymentSnapshot{ProviderRef: ref, Status: connector.PaymentSucceeded, Amount: 50000, Currency: "INR"}}}

		// No signature, or a wrong one: 401, stored verified=false, no job.
		for name, sig := range map[string]string{"missing": "", "wrong": "nope"} {
			code, _ := e.hook(st.AccountID, "evt_forged", sig, notes)
			if code != 401 {
				t.Errorf("%s signature: %d, want 401", name, code)
			}
		}
		if e.count("SELECT COUNT(*) FROM jobs WHERE kind IN ('provider_event.process', 'provider_event.fetch_only')") != 0 {
			t.Error("a forged webhook enqueued processing")
		}
		if e.count("SELECT COUNT(*) FROM provider_events WHERE verified = 0 AND error LIKE 'verification failed%'") == 0 {
			t.Error("forged webhook not stored as unverified")
		}
		if e.count("SELECT COUNT(*) FROM provider_events WHERE verified = 1") != 0 {
			t.Error("forged webhook stored as verified")
		}
		for i := 0; i < 3; i++ {
			e.clk.Add(2 * time.Minute)
			_, _ = e.q.RunOnce(ctx)
		}
		e.fake.Locked(func() {
			if len(e.fake.Fetches) != 0 {
				t.Errorf("forged webhook triggered %d fetches", len(e.fake.Fetches))
			}
		})
		if got := e.get(p.ID); got.Status == "succeeded" {
			t.Fatal("a forged webhook changed state")
		}

		// A genuinely unsigned event type (the provider never signs it) still
		// takes the fetch-only path.
		notes[0].EventID = "evt_unsigned"
		code, _ := e.hook(st.AccountID, "evt_unsigned", "unsigned", notes)
		if code != 200 || e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.fetch_only'") != 1 {
			t.Errorf("unsigned event type: %d", code)
		}
		// A signed one is processed normally.
		notes[0].EventID = "evt_ok" // dedupe is per provider event id
		code, _ = e.hook(st.AccountID, "evt_ok", "ok", notes)
		if code != 200 || e.count("SELECT COUNT(*) FROM jobs WHERE kind = 'provider_event.process'") != 1 {
			t.Errorf("signed: %d", code)
		}
	})
}

// ---- disputes ----

type stubDisputes struct {
	err   error
	calls []string
}

func (d *stubDisputes) SyncNotification(_ context.Context, account string, n connector.Notification) (disputes.Result, error) {
	d.calls = append(d.calls, account+"/"+n.Dispute.ProviderDisputeID)
	return disputes.Result{}, d.err
}

func TestDisputeNotificationsGoToTheDisputesService(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		stub := &stubDisputes{}
		flow := checkout.New(checkout.Options{DB: e.db, Core: e.core, Conns: e.conns, Routing: checkout.NewRouting(e.breaker), Breaker: e.breaker,
			Now: e.clk.Now, Disputes: stub})
		_, st := startedAttempt(t, e)
		note := []connector.Notification{{Kind: connector.NotifyDispute, EventID: "d1",
			Dispute: &connector.DisputeSnapshot{ProviderDisputeID: "dsp_1", ProviderPaymentID: "pay_1"}}}
		ev := storeEvent(t, e, st.AccountID, true, note)
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
			t.Fatal(err)
		}
		if len(stub.calls) != 1 || stub.calls[0] != st.AccountID+"/dsp_1" {
			t.Errorf("calls %v", stub.calls)
		}
		if e.count("SELECT COUNT(*) FROM unknown_provider_objects") != 0 {
			t.Error("a handled dispute was recorded as unknown")
		}
		// A dispute of no known payment, or for a connector that cannot read
		// disputes, is recorded for an operator instead of failing the job.
		for i, serr := range []error{disputes.ErrUnknownPayment, disputes.ErrNoReader} {
			stub.err = serr
			note[0].Dispute.ProviderDisputeID = fmt.Sprintf("dsp_u%d", i)
			ev := storeEvent(t, e, st.AccountID, true, note)
			time.Sleep(time.Millisecond)
			if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err != nil {
				t.Fatalf("%v: %v", serr, err)
			}
		}
		if e.count("SELECT COUNT(*) FROM unknown_provider_objects WHERE kind = 'dispute'") != 2 {
			t.Errorf("unknown disputes recorded: %d", e.count("SELECT COUNT(*) FROM unknown_provider_objects WHERE kind = 'dispute'"))
		}
		// Anything else is retried.
		stub.err = errors.New("db down")
		note[0].Dispute.ProviderDisputeID = "dsp_retry"
		ev = storeEvent(t, e, st.AccountID, true, note)
		if err := flow.ProcessProviderEvent(ctx, jobPayload(st.AccountID, ev)); err == nil {
			t.Error("a transient failure must make the job retry")
		}
	})
}
