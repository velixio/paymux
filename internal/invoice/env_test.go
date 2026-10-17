package invoice_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/events"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/notify"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
)

var ctx = context.Background()

const (
	supplierGSTIN = "27AAPFU0939F1ZV" // Maharashtra
	baseURL       = "https://pay.example.com"
)

// validGSTIN completes a 14-character GSTIN prefix with its checksum.
func validGSTIN(t testing.TB, prefix14 string) string {
	t.Helper()
	for _, c := range "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		if g := prefix14 + string(c); tax.ValidGSTIN(g) == nil {
			return g
		}
	}
	t.Fatalf("no checksum for %s", prefix14)
	return ""
}

type recMailer struct {
	mu   sync.Mutex
	sent []notify.Message
	err  error
}

func (m *recMailer) Send(_ context.Context, msg notify.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *recMailer) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.sent) }

// recAlerts records the alerts the invoice service raises.
type recAlerts struct {
	mu  sync.Mutex
	got []notify.Alert
}

func (r *recAlerts) Raise(_ context.Context, al notify.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, al)
	return nil
}

func (r *recAlerts) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.got {
		out = append(out, a.Kind)
	}
	return out
}

type env struct {
	alerts *recAlerts
	t      *testing.T
	db     *store.DB
	core   *core.Service
	inv    *invoice.Service
	q      *jobs.Queue
	set    *settings.Service
	mailer *recMailer
	scope  core.Scope
	now    time.Time
	mu     sync.Mutex
	events []events.Event
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

func (e *env) setNow(t time.Time) { e.mu.Lock(); e.now = t; e.mu.Unlock() }

func (e *env) emittedTypes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, ev := range e.events {
		out = append(out, ev.Type)
	}
	return out
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

type tweak func(*store.TaxEntity, *store.AppTaxProfile)

// newEnv builds a live-mode world: tax entity te_1 (Maharashtra, GSTIN
// supplierGSTIN, LUT valid through the whole of 2026-27) bound to app_1 with
// SAC 998315 at 18% inclusive, a normal connector account and a
// merchant-of-record one.
func newEnv(t *testing.T, db *store.DB, tw ...tweak) *env {
	t.Helper()
	e := &env{t: t, db: db, set: settings.New(db), mailer: &recMailer{}, alerts: &recAlerts{}, now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	e.q = jobs.New(db, jobs.Options{})
	emitter := events.EmitterFunc(func(_ context.Context, _ store.Tx, ev events.Event) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.events = append(e.events, ev)
		return nil
	})
	box, err := secrets.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.inv = invoice.New(invoice.Options{Box: box, DB: db, Settings: e.set, Jobs: e.q, Emitter: emitter, Mailer: e.mailer, BaseURL: baseURL, Alerter: e.alerts,
		Now: e.clock, IsMerchantOfRecord: func(name string) bool { return name == "mor" }})
	e.inv.RegisterJobs(e.q)
	e.core = core.NewService(core.Options{DB: db, Settings: e.set, Jobs: e.q, Emitter: emitter, Now: e.clock,
		OnPaymentSucceeded: e.inv.OnPaymentSucceeded, OnRefundSucceeded: e.inv.OnRefundSucceeded})

	ent := store.TaxEntity{ID: "te_1", OrgID: store.DefaultOrgID, LegalName: "Voice Studio Pvt Ltd", TradeName: "Voicestudio",
		GSTIN: supplierGSTIN, PAN: "AAPFU0939F", Address: "1 MG Road, Mumbai 400001", StateCode: "27",
		LUTNumber: "AD270226000001A", LUTValidFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, tax.IST), LUTValidTo: time.Date(2027, 3, 31, 0, 0, 0, 0, tax.IST),
		InvoicePrefix: "INV", CreditNotePrefix: "CN", SignatoryName: "A. Signatory", FooterNote: "Thank you", SupportEmail: "help@example.com",
		CreatedAt: e.now, UpdatedAt: e.now}
	prof := store.AppTaxProfile{AppID: "app_1", TaxEntityID: "te_1", IssueInvoices: true, SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: true}
	for _, f := range tw {
		f(&ent, &prof)
	}
	err = db.Write(ctx, func(tx store.Tx) error {
		if err := store.SeedOrg(ctx, tx, "Paymux", e.now); err != nil {
			return err
		}
		if err := store.InsertApp(ctx, tx, store.App{ID: "app_1", OrgID: store.DefaultOrgID, Name: "Voicestudio", Slug: "vs",
			DefaultReturnURL: "https://app.example.com/return", CreatedAt: e.now}); err != nil {
			return err
		}
		for _, pa := range []store.ProviderAccount{
			{ID: "pa_1", OrgID: store.DefaultOrgID, Mode: "live", Connector: "fake", Name: "Fake", Status: "active", CreatedAt: e.now},
			{ID: "pa_mor", OrgID: store.DefaultOrgID, Mode: "live", Connector: "mor", Name: "MoR", Status: "active", CreatedAt: e.now},
			{ID: "pa_test", OrgID: store.DefaultOrgID, Mode: "test", Connector: "fake", Name: "FakeT", Status: "active", CreatedAt: e.now},
		} {
			if err := store.InsertProviderAccount(ctx, tx, pa); err != nil {
				return err
			}
		}
		if err := store.InsertTaxEntity(ctx, tx, ent); err != nil {
			return err
		}
		return store.UpsertAppTaxProfile(ctx, tx, prof, e.now)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.set.SeedOrg(ctx, store.DefaultOrgID); err != nil {
		t.Fatal(err)
	}
	e.scope = core.Scope{AppID: "app_1", Mode: "live"}
	return e
}

// billing shortcuts
var (
	intraB2C = &core.CustomerInput{Name: "Asha", Email: "asha@example.com", Billing: store.Billing{StateCode: "27", Country: "IN", Line1: "5 FC Road", City: "Pune", PostalCode: "411004"}}
	interB2C = &core.CustomerInput{Name: "Ravi", Email: "ravi@example.com", Billing: store.Billing{StateCode: "29", Country: "IN"}}
	noState  = &core.CustomerInput{Name: "Nina", Email: "nina@example.com"}
	usBuyer  = &core.CustomerInput{Name: "Bob", Email: "bob@example.com", Billing: store.Billing{Country: "US", Line1: "1 Main St", City: "Austin"}}
)

// pay creates a payment for the customer, an attempt on providerAccount, and
// applies a captured snapshot, so the invoice hook runs.
func (e *env) pay(amount int64, currency string, cust *core.CustomerInput, providerAccount string) (store.Payment, store.Attempt) {
	e.t.Helper()
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: e.scope, Amount: amount, Currency: currency,
		OrderRef: "ord", Description: "Voicestudio Pro - Oct 2026", Customer: cust})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.core.CreateAttempt(ctx, res.Payment.ID, providerAccount)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: res.Payment.Amount, Currency: currency,
		Method: "upi", ProviderPaymentID: "pp_" + a.ID}); err != nil {
		e.t.Fatal(err)
	}
	p, err := e.core.GetPayment(ctx, e.scope, res.Payment.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	a, _ = e.core.GetAttempt(ctx, a.ID)
	return p, a
}

func (e *env) invoices(paymentID string) []store.Invoice {
	e.t.Helper()
	var out []store.Invoice
	if err := e.db.Read(ctx, func(q store.Querier) (err error) {
		out, err = store.InvoicesForPayment(ctx, q, paymentID)
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) creditNotes(paymentID string) []store.CreditNote {
	e.t.Helper()
	var out []store.CreditNote
	if err := e.db.Read(ctx, func(q store.Querier) (err error) {
		out, err = store.CreditNotesForPayment(ctx, q, paymentID)
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) setFX(currency, date, rate string) {
	e.t.Helper()
	if err := e.inv.SetFXRate(ctx, audit.System, currency, date, rate, "manual"); err != nil {
		e.t.Fatal(err)
	}
}

// refund creates a refund and marks it succeeded the way the refunds lane
// will: update the row, then call the core hook in the same transaction.
func (e *env) refund(p store.Payment, amount int64) store.Refund {
	e.t.Helper()
	r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: amount, Reason: "customer request"})
	if err != nil {
		e.t.Fatal(err)
	}
	err = e.db.Write(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE refunds SET status = 'succeeded' WHERE id = ?", r.ID); err != nil {
			return err
		}
		r.Status = "succeeded"
		return e.core.RefundSucceeded(ctx, tx, r)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// runJobs drains the job queue.
func (e *env) runJobs() {
	e.t.Helper()
	for i := 0; i < 20; i++ {
		n, err := e.q.RunOnce(ctx)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (e *env) setDelivery(mode string) {
	e.t.Helper()
	if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "invoice_delivery", []byte(`"`+mode+`"`)); err != nil {
		e.t.Fatal(err)
	}
}
