package subscriptions_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/subscriptions"
	"github.com/paymux/paymux/pkg/connector"
)

var ctx = context.Background()

const supplierGSTIN = "27AAPFU0939F1ZV"

// rec records what the fake connectors were asked to do.
type rec struct {
	mu          sync.Mutex
	charges     []connector.ChargeRequest
	notices     []connector.PreDebitRequest
	revoked     []string
	mandateReqs []connector.MandateRequest
	subReqs     []connector.SubscriptionRequest
	canceled    []string
	updated     []int64
	mandates    map[string]connector.MandateSnapshot
	chargeFn    func(connector.ChargeRequest) (connector.PaymentSnapshot, error)
	chargeDelay time.Duration
	noticeErr   error
	payFn       func(connector.PaymentRef) (connector.PaymentSnapshot, error)
	perAttempt  bool
	nsub        int
}

func (r *rec) chargeCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.charges) }

type base struct {
	name   string
	modes  []connector.BillingMode
	limits []connector.MandateLimit
	r      *rec
}

func (b base) Info() connector.Info {
	return connector.Info{Name: b.name, Version: "1", DisplayName: b.name, Currencies: []string{"INR"}, SubscriptionModes: b.modes,
		MandateLimits: b.limits, PreDebitPerAttempt: b.r.perAttempt}
}
func (b base) CreateCheckout(context.Context, connector.CheckoutRequest) (connector.CheckoutResult, error) {
	return connector.CheckoutResult{}, connector.ErrUnsupported
}
func (b base) HandleReturn(context.Context, connector.ReturnRequest) (connector.ReturnResult, error) {
	return connector.ReturnResult{}, connector.ErrUnsupported
}
func (b base) VerifyWebhook(context.Context, connector.WebhookRequest) error { return nil }
func (b base) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	return nil, nil
}
func (b base) FetchPayment(_ context.Context, ref connector.PaymentRef) (connector.PaymentSnapshot, error) {
	b.r.mu.Lock()
	fn := b.r.payFn
	b.r.mu.Unlock()
	if fn != nil {
		return fn(ref)
	}
	return connector.PaymentSnapshot{}, connector.ErrNotFound
}

type mandateConn struct{ base }

func (c mandateConn) CreateMandateCheckout(_ context.Context, req connector.MandateRequest) (connector.CheckoutResult, error) {
	c.r.mu.Lock()
	c.r.mandateReqs = append(c.r.mandateReqs, req)
	c.r.mu.Unlock()
	return connector.CheckoutResult{NextAction: connector.NextAction{Kind: connector.ActionFormPost, URL: "https://pg.example.com/si",
		Fields: map[string]string{"txnid": req.ProviderRef, "max": fmt.Sprint(req.MaxAmount)}}}, nil
}
func (c mandateConn) FetchMandate(_ context.Context, ref connector.MandateRef) (connector.MandateSnapshot, error) {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	if m, ok := c.r.mandates[ref.ProviderMandateID]; ok {
		return m, nil
	}
	return connector.MandateSnapshot{}, connector.ErrNotFound
}
func (c mandateConn) NotifyPreDebit(_ context.Context, req connector.PreDebitRequest) error {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	if c.r.noticeErr != nil {
		return c.r.noticeErr
	}
	c.r.notices = append(c.r.notices, req)
	return nil
}
func (c mandateConn) ChargeMandate(_ context.Context, req connector.ChargeRequest) (connector.PaymentSnapshot, error) {
	c.r.mu.Lock()
	c.r.charges = append(c.r.charges, req)
	fn, d := c.r.chargeFn, c.r.chargeDelay
	c.r.mu.Unlock()
	time.Sleep(d)
	if fn != nil {
		return fn(req)
	}
	return connector.PaymentSnapshot{Status: connector.PaymentSucceeded, Amount: req.Amount, Currency: req.Currency,
		ProviderPaymentID: "pp_" + req.ProviderRef, Method: "upi"}, nil
}
func (c mandateConn) RevokeMandate(_ context.Context, ref connector.MandateRef) error {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.revoked = append(c.r.revoked, ref.ProviderMandateID)
	return nil
}

type managedConn struct{ base }

func (c managedConn) CreateSubscriptionCheckout(_ context.Context, req connector.SubscriptionRequest) (connector.CheckoutResult, error) {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.subReqs = append(c.r.subReqs, req)
	c.r.nsub++
	return connector.CheckoutResult{ProviderOrderID: fmt.Sprintf("sub_prov_%d", c.r.nsub),
		NextAction: connector.NextAction{Kind: connector.ActionSDK, ScriptURL: "https://checkout.example.com/c.js",
			Options: map[string]any{"subscription_id": fmt.Sprintf("sub_prov_%d", c.r.nsub)}}}, nil
}
func (c managedConn) FetchSubscription(_ context.Context, ref connector.SubscriptionRef) (connector.SubscriptionSnapshot, error) {
	return connector.SubscriptionSnapshot{ProviderSubscriptionID: ref.ProviderSubscriptionID, Status: "active", RawStatus: "active"}, nil
}
func (c managedConn) CancelSubscription(_ context.Context, ref connector.SubscriptionRef, atEnd bool) error {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.canceled = append(c.r.canceled, fmt.Sprintf("%s:%t", ref.ProviderSubscriptionID, atEnd))
	return nil
}
func (c managedConn) UpdateSubscriptionAmount(_ context.Context, _ connector.SubscriptionRef, amount int64) error {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.updated = append(c.r.updated, amount)
	return nil
}

var (
	allMethods = []connector.MandateLimit{{Method: "upi", Currency: "INR"}, {Method: "card", Currency: "INR"}}
	mandateMod = []connector.BillingMode{connector.MerchantTriggered}
	managedMod = []connector.BillingMode{connector.ProviderManaged}
)

type env struct {
	t      *testing.T
	db     *store.DB
	core   *core.Service
	inv    *invoice.Service
	subs   *subscriptions.Service
	q      *jobs.Queue
	set    *settings.Service
	r      *rec
	scope  core.Scope
	mu     sync.Mutex
	now    time.Time
	events []events.Event
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) setNow(t time.Time) {
	e.mu.Lock()
	e.now = t
	e.mu.Unlock()
}

func (e *env) eventsOf(typ string) []events.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []events.Event
	for _, ev := range e.events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) eventTypes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		out = append(out, ev.Type)
	}
	return out
}

// newEnv builds a live-mode world at start: app_1 with GST profile, a UPI/card
// mandate account (connector "payu"), an e-NACH account ("zoho") and a
// provider-managed account ("razorpay"). Routing defaults to payu only; use
// routeTo to change it.
func newEnv(t *testing.T, db *store.DB, start time.Time) *env {
	t.Helper()
	e := &env{t: t, db: db, set: settings.New(db), now: start, r: &rec{mandates: map[string]connector.MandateSnapshot{}}}
	e.q = jobs.New(db, jobs.Options{Clock: e.clock, Jitter: func() float64 { return 0 }})
	emitter := events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.events = append(e.events, ev)
		return nil
	})
	e.inv = invoice.New(invoice.Options{DB: db, Settings: e.set, Jobs: e.q, Emitter: emitter, BaseURL: "https://pay.example.com", Now: e.clock,
		IsMerchantOfRecord: func(string) bool { return false }})
	e.inv.RegisterJobs(e.q)
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Jobs: e.q, Emitter: emitter, Now: e.clock,
		OnPaymentSucceeded: e.inv.OnPaymentSucceeded, OnRefundSucceeded: e.inv.OnRefundSucceeded})
	conns := map[string]connector.Connector{
		"pa_upi":     mandateConn{base{name: "payu", modes: mandateMod, limits: allMethods, r: e.r}},
		"pa_enach":   mandateConn{base{name: "zoho", modes: mandateMod, limits: []connector.MandateLimit{{Method: "emandate", Currency: "INR"}}, r: e.r}},
		"pa_managed": managedConn{base{name: "razorpay", modes: managedMod, r: e.r}},
	}
	e.subs = subscriptions.New(subscriptions.Options{DB: db, Core: e.core, Settings: e.set, Jobs: e.q, Emitter: emitter, BaseURL: "https://pay.example.com",
		Now: e.clock, Connectors: subscriptions.ConnectorsFunc(func(_ context.Context, id string) (connector.Connector, error) {
			if c, ok := conns[id]; ok {
				return c, nil
			}
			return nil, fmt.Errorf("no connector for %s", id)
		})})
	e.subs.RegisterJobs(e.q)

	err := db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", start); err != nil {
			return err
		}
		if err := store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "Voicestudio", Slug: "vs",
			DefaultReturnURL: "https://app.example.com/done", CreatedAt: start}); err != nil {
			return err
		}
		for _, a := range [][2]string{{"pa_upi", "payu"}, {"pa_enach", "zoho"}, {"pa_managed", "razorpay"}} {
			if err := store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: a[0], OrgID: store.DefaultOrgID, Mode: "live",
				Connector: a[1], Name: a[0], Status: "active", CreatedAt: start}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE provider_accounts SET enabled_purposes = '["one_time","subscription"]',
				accepted_currencies = '["INR"]' WHERE id = ?`, a[0]); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO app_provider_accounts (app_id, provider_account_id) VALUES ('app_1', ?)", a[0]); err != nil {
				return err
			}
		}
		if err := store.InsertTaxEntity(ctx, tx, store.TaxEntity{ID: "te_1", OrgID: store.DefaultOrgID, LegalName: "Voice Studio Pvt Ltd",
			GSTIN: supplierGSTIN, StateCode: "27", InvoicePrefix: "INV", CreditNotePrefix: "CN", CreatedAt: start, UpdatedAt: start}); err != nil {
			return err
		}
		return store.UpsertAppTaxProfile(ctx, tx, store.AppTaxProfile{AppID: "app_1", TaxEntityID: "te_1", IssueInvoices: true,
			SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: true}, start)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
		t.Fatal(err)
	}
	e.scope = core.Scope{AppID: "app_1", Mode: "live"}
	e.routeTo("payu")
	return e
}

func (e *env) routeTo(connectors ...string) {
	e.t.Helper()
	b, _ := json.Marshal(connectors)
	raw := fmt.Sprintf(`{"rules":[{"currency":"INR","connectors":%s}],"catch_all":{"connectors":["razorpay"]}}`, b)
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", []byte(raw)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setSetting(key, raw string) {
	e.t.Helper()
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, key, []byte(raw)); err != nil {
		e.t.Fatal(err)
	}
}

var customer = &core.CustomerInput{ExternalRef: "u1", Name: "Asha", Email: "asha@example.com",
	Billing: store.Billing{StateCode: "27", Country: "IN"}}

func (e *env) create(mut func(*subscriptions.CreateInput)) (subscriptions.Created, error) {
	in := subscriptions.CreateInput{Scope: e.scope, Customer: customer, Amount: 49900, Currency: "INR", Interval: "month",
		Description: "Pro", Method: "upi"}
	if mut != nil {
		mut(&in)
	}
	return e.subs.Create(ctx, in)
}

func (e *env) mustCreate(mut func(*subscriptions.CreateInput)) subscriptions.Created {
	e.t.Helper()
	c, err := e.create(mut)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) sub(id string) store.Subscription {
	e.t.Helper()
	var s store.Subscription
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { s, err = store.GetSubscription(ctx, q, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) mandate(id string) store.Mandate {
	e.t.Helper()
	var m store.Mandate
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { m, err = store.GetMandate(ctx, q, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) cycles(subID string) []store.Cycle {
	e.t.Helper()
	var out []store.Cycle
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.ListCycles(ctx, q, subID); return }); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// activate completes the mandate setup of a merchant-triggered subscription
// the way the provider's notification would.
func (e *env) activateMandate(c subscriptions.Created) store.Subscription {
	e.t.Helper()
	sub := e.sub(c.Subscription.ID)
	m := e.mandate(sub.MandateID)
	pid := "pm_" + m.ID
	e.activateMandateByID(m, pid)
	return e.sub(sub.ID)
}

func (e *env) activateMandateByID(m store.Mandate, providerID string) {
	e.t.Helper()
	// The mandate must be known by its provider id: the return flow learns it.
	e.r.mu.Lock()
	e.r.mandates[providerID] = connector.MandateSnapshot{ProviderMandateID: providerID, Status: "active", RawStatus: "confirmed",
		MaxAmount: m.MaxAmount, Currency: "INR", Method: m.Method}
	e.r.mu.Unlock()
	if err := e.db.Write(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE mandates SET provider_mandate_id = ? WHERE id = ?", providerID, m.ID)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.subs.HandleNotification(ctx, m.ProviderAccountID, connector.Notification{Kind: connector.NotifyMandate, EventID: "ev_" + providerID,
		Mandate: &connector.MandateSnapshot{ProviderMandateID: providerID, Status: "active", RawStatus: "confirmed"}}); err != nil {
		e.t.Fatal(err)
	}
}

// runJobs runs due jobs until none are left.
func (e *env) runJobs() {
	e.t.Helper()
	for i := 0; i < 60; i++ {
		n, err := e.q.RunOnce(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	e.t.Fatal("jobs keep coming")
}

// advanceTo sets the clock and runs everything that became due.
func (e *env) advanceTo(t time.Time) {
	e.t.Helper()
	e.setNow(t)
	e.runJobs()
}

func (e *env) pendingJobs(kind string) []time.Time {
	e.t.Helper()
	var out []time.Time
	err := e.db.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT run_at FROM jobs WHERE kind = ? AND state = 'pending' ORDER BY run_at", kind)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ms int64
			if err := rows.Scan(&ms); err != nil {
				return err
			}
			out = append(out, time.UnixMilli(ms).UTC())
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) renewalPayments(subID string) []store.Payment {
	e.t.Helper()
	var out []store.Payment
	err := e.db.Read(ctx, func(q store.Querier) error {
		rows, err := q.QueryContext(ctx, "SELECT id FROM payments WHERE subscription_id = ? ORDER BY id", subID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			p, err := store.GetPayment(ctx, q, id)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}
