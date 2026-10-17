package httpapi_test

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/internal/httpapi"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/server"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// invoiceFixture is a live-mode world with the invoice hooks wired.
type invoiceFixture struct {
	*fixture
	q   *jobs.Queue
	inv *invoice.Service
}

func newInvoiceFixture(t *testing.T, db *store.DB) *invoiceFixture {
	t.Helper()
	set := settings.New(db)
	if _, err := core.Bootstrap(ctx, db, set); err != nil {
		t.Fatal(err)
	}
	q := jobs.New(db, jobs.Options{})
	inv := invoice.New(invoice.Options{DB: db, Settings: set, Jobs: q, BaseURL: "https://pay.example.com",
		IsMerchantOfRecord: func(string) bool { return false }})
	inv.RegisterJobs(q)
	svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: q,
		OnPaymentSucceeded: inv.OnPaymentSucceeded, OnRefundSucceeded: inv.OnRefundSucceeded})
	f := &fixture{t: t, db: db, svc: svc, pa: "pa_live"}
	now := time.Now()
	var err error
	f.app, err = svc.CreateApp(ctx, audit.System, core.CreateAppInput{Name: "One", Slug: "one"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateApp(ctx, audit.System, core.CreateAppInput{Name: "Two", Slug: "two"})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Write(ctx, func(tx store.Tx) error {
		if err := store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_live", OrgID: store.DefaultOrgID, Mode: "live",
			Connector: "fake", Name: "Fake", Status: "active", CreatedAt: now}); err != nil {
			return err
		}
		if err := store.InsertTaxEntity(ctx, tx, store.TaxEntity{ID: "te_1", OrgID: store.DefaultOrgID, LegalName: "Voice Studio",
			GSTIN: "27AAPFU0939F1ZV", StateCode: "27", InvoicePrefix: "INV", CreditNotePrefix: "CN", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return store.UpsertAppTaxProfile(ctx, tx, store.AppTaxProfile{AppID: f.app.ID, TaxEntityID: "te_1", IssueInvoices: true,
			SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: true}, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.liveKey, _, _ = svc.CreateAPIKey(ctx, audit.System, f.app.ID, "live")
	f.testKey, _, _ = svc.CreateAPIKey(ctx, audit.System, f.app.ID, "test")
	f.otherKey, _, _ = svc.CreateAPIKey(ctx, audit.System, other.ID, "live")
	box, _ := secrets.NewBox(bytes.Repeat([]byte{7}, 32))
	base, _ := url.Parse("https://pay.example.com")
	api, err := httpapi.New(httpapi.Options{Core: svc, DB: db, Box: box, BaseURL: base, Invoices: inv})
	if err != nil {
		t.Fatal(err)
	}
	s := server.New(server.Options{})
	api.Register(s)
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return &invoiceFixture{fixture: f, q: q, inv: inv}
}

func TestInvoiceAndCreditNoteAPI(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		f := newInvoiceFixture(t, db)
		r := f.do("POST", "/v1/payments", f.liveKey, map[string]any{"amount": 118000, "currency": "INR", "description": "Pro",
			"customer": map[string]any{"name": "Asha", "billing": map[string]any{"state_code": "27", "country": "IN"}}})
		if r.status != 201 {
			t.Fatalf("create: %d %s", r.status, r.raw)
		}
		pid := r.json(t)["id"].(string)
		a, err := f.svc.CreateAttempt(ctx, pid, "pa_live")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: 118000, Currency: "INR", Method: "upi"}); err != nil {
			t.Fatal(err)
		}

		// List by payment, then get.
		l := f.do("GET", "/v1/invoices?payment_id="+pid, f.liveKey, nil)
		if l.status != 200 {
			t.Fatalf("list: %d %s", l.status, l.raw)
		}
		data := l.json(t)["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("list = %s", l.raw)
		}
		inv := data[0].(map[string]any)
		id := inv["id"].(string)
		if !strings.HasPrefix(inv["number"].(string), "INV/") || inv["total"] != float64(118000) ||
			inv["cgst"] != float64(9000) || inv["object"] != "invoice" || inv["pdf_ready"] != false || inv["payment_id"] != pid {
			t.Errorf("invoice = %v", inv)
		}
		if _, leaked := inv["access_token_hash"]; leaked || strings.Contains(string(l.raw), "access_token") {
			t.Error("access token leaked")
		}
		if g := f.do("GET", "/v1/invoices/"+id, f.liveKey, nil); g.status != 200 || g.json(t)["id"] != id {
			t.Errorf("get: %d %s", g.status, g.raw)
		}

		// PDF: 404 until rendered, then bytes.
		if p := f.do("GET", "/v1/invoices/"+id+"/pdf", f.liveKey, nil); p.status != 404 || p.header.Get("Retry-After") == "" {
			t.Errorf("pdf before render: %d", p.status)
		}
		for i := 0; i < 5; i++ {
			if n, _ := f.q.RunOnce(ctx); n == 0 {
				break
			}
		}
		p := f.do("GET", "/v1/invoices/"+id+"/pdf", f.liveKey, nil)
		if p.status != 200 || p.header.Get("Content-Type") != "application/pdf" || !bytes.HasPrefix(p.raw, []byte("%PDF-")) {
			t.Fatalf("pdf: %d %v", p.status, p.header)
		}

		// Tenant and mode scoping.
		for _, c := range []struct{ name, key string }{{"other app", f.otherKey}, {"test key", f.testKey}} {
			if g := f.do("GET", "/v1/invoices/"+id, c.key, nil); g.status != 404 || g.errType(t) != "not_found" {
				t.Errorf("%s: %d %s", c.name, g.status, g.raw)
			}
			if g := f.do("GET", "/v1/invoices/"+id+"/pdf", c.key, nil); g.status != 404 {
				t.Errorf("%s pdf: %d", c.name, g.status)
			}
			if l := f.do("GET", "/v1/invoices?payment_id="+pid, c.key, nil); l.status != 200 || len(l.json(t)["data"].([]any)) != 0 {
				t.Errorf("%s list: %d %s", c.name, l.status, l.raw)
			}
		}
		if g := f.do("GET", "/v1/invoices", f.liveKey, nil); g.status != 400 || g.errType(t) != "invalid_request" {
			t.Errorf("list without payment_id: %d %s", g.status, g.raw)
		}
		if g := f.do("GET", "/v1/invoices/"+id, "", nil); g.status != 401 {
			t.Errorf("unauthenticated: %d", g.status)
		}

		// Credit note from a succeeded refund.
		ref, err := f.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: core.Scope{AppID: f.app.ID, Mode: "live"}, Actor: audit.System,
			InitiatedBy: "api", PaymentID: pid, Amount: 59000, Reason: "dup"})
		if err != nil {
			t.Fatal(err)
		}
		err = db.Write(ctx, func(tx store.Tx) error {
			if _, err := tx.ExecContext(ctx, "UPDATE refunds SET status = 'succeeded' WHERE id = ?", ref.ID); err != nil {
				return err
			}
			return f.svc.RefundSucceeded(ctx, tx, ref)
		})
		if err != nil {
			t.Fatal(err)
		}
		cl := f.do("GET", "/v1/credit_notes?payment_id="+pid, f.liveKey, nil)
		cdata := cl.json(t)["data"].([]any)
		if cl.status != 200 || len(cdata) != 1 {
			t.Fatalf("credit notes: %d %s", cl.status, cl.raw)
		}
		cn := cdata[0].(map[string]any)
		if cn["object"] != "credit_note" || cn["total"] != float64(59000) || cn["invoice_id"] != id || cn["refund_id"] != ref.ID || cn["reason"] != "dup" {
			t.Errorf("credit note = %v", cn)
		}
		cid := cn["id"].(string)
		if g := f.do("GET", "/v1/credit_notes/"+cid, f.liveKey, nil); g.status != 200 {
			t.Errorf("get credit note: %d", g.status)
		}
		if g := f.do("GET", "/v1/credit_notes/"+cid, f.otherKey, nil); g.status != 404 {
			t.Errorf("other app credit note: %d", g.status)
		}
		for i := 0; i < 5; i++ {
			if n, _ := f.q.RunOnce(ctx); n == 0 {
				break
			}
		}
		if p := f.do("GET", "/v1/credit_notes/"+cid+"/pdf", f.liveKey, nil); p.status != 200 || !bytes.HasPrefix(p.raw, []byte("%PDF-")) {
			t.Errorf("credit note pdf: %d", p.status)
		}
	})
}

func TestInvoiceRoutesWithoutServiceAre404(t *testing.T) {
	each(t, func(t *testing.T, f *fixture) {
		if g := f.do("GET", "/v1/invoices/inv_x", f.testKey, nil); g.status != 404 {
			t.Errorf("status = %d", g.status)
		}
	})
}
