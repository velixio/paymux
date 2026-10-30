package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
	"github.com/velixio/paymux/internal/invoice"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/tax"
)

const supplierGSTIN = "27AAPFU0939F1ZV" // Maharashtra

// gstin completes a 14-character GSTIN prefix with its checksum.
func gstin(t testing.TB, prefix14 string) string {
	t.Helper()
	for _, c := range "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		if g := prefix14 + string(c); tax.ValidGSTIN(g) == nil {
			return g
		}
	}
	t.Fatalf("no checksum for %s", prefix14)
	return ""
}

var (
	intraB2C = &core.CustomerInput{Name: "Asha", Email: "asha@example.com", Billing: store.Billing{StateCode: "27", Country: "IN", Line1: "5 FC Road", City: "Pune", PostalCode: "411004"}}
	interB2C = &core.CustomerInput{Name: "Ravi", Email: "ravi@example.com", Billing: store.Billing{StateCode: "29", Country: "IN"}}
	noState  = &core.CustomerInput{Name: "Nina", Email: "nina@example.com"}
	noEmail  = &core.CustomerInput{Name: "Nobody", Billing: store.Billing{StateCode: "27", Country: "IN"}}
	usBuyer  = &core.CustomerInput{Name: "Bob", Email: "bob@example.com", Billing: store.Billing{Country: "US", Line1: "1 Main St", City: "Austin"}}
)

// taxApp creates an app that issues invoices under a tax entity with a live
// provider account to pay through.
func (e *env) taxApp(slug, entityGSTIN string) (store.App, store.TaxEntity) {
	e.t.Helper()
	app := e.seedApp(slug)
	ent, err := e.inv.FindTaxEntity(ctx, entityGSTIN)
	if err != nil {
		ent, err = e.inv.AddTaxEntity(ctx, audit.System, invoice.TaxEntityInput{LegalName: "Legal " + slug, TradeName: "Trade " + slug, GSTIN: entityGSTIN,
			Address: "1 MG Road, Mumbai 400001", LUTNumber: "AD270226000001A", LUTFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			LUTTo: time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC), SignatoryName: "A. Signatory", SupportEmail: "help@example.com"})
		if err != nil {
			e.t.Fatal(err)
		}
	}
	if _, err := e.inv.SetAppTaxProfile(ctx, audit.System, invoice.AppTaxInput{AppID: app.ID, TaxEntityID: ent.ID, IssueInvoices: true,
		SACCode: "998315", GSTRateBP: 1800, PricesIncludeTax: true}); err != nil {
		e.t.Fatal(err)
	}
	err = e.db.Write(ctx, func(tx store.Tx) error {
		if _, err := store.GetProviderAccount(ctx, tx, "pa_live"); err == nil {
			return nil
		}
		return store.InsertProviderAccount(ctx, tx, store.ProviderAccount{ID: "pa_live", OrgID: store.DefaultOrgID, Mode: "live", Connector: "fake",
			Name: "Fake live", Status: "active", CreatedAt: e.clk.Now()})
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return app, ent
}

// pay creates and captures a payment, which issues the invoice.
func (e *env) pay(app store.App, amount int64, currency string, cust *core.CustomerInput) store.Payment {
	return e.payVia(app, amount, currency, cust, "pa_live")
}

// payVia is pay through a chosen provider account.
func (e *env) payVia(app store.App, amount int64, currency string, cust *core.CustomerInput, accountID string) store.Payment {
	e.t.Helper()
	sc := core.Scope{AppID: app.ID, Mode: "live"}
	res, err := e.core.CreatePayment(ctx, core.CreatePaymentInput{Scope: sc, Amount: amount, Currency: currency, OrderRef: "ord", Description: "Pro plan", Customer: cust})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.core.CreateAttempt(ctx, res.Payment.ID, accountID)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.core.ApplyAttemptSnapshot(ctx, a.ID, core.Snapshot{Status: state.AttemptCaptured, Amount: amount, Currency: currency, Method: "upi",
		ProviderPaymentID: "pp_" + a.ID}); err != nil {
		e.t.Fatal(err)
	}
	p, err := e.core.GetPayment(ctx, sc, res.Payment.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) invoicesOf(paymentID string) []store.Invoice {
	e.t.Helper()
	var out []store.Invoice
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.InvoicesForPayment(ctx, q, paymentID); return }); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) creditNotesOf(paymentID string) []store.CreditNote {
	e.t.Helper()
	var out []store.CreditNote
	if err := e.db.Read(ctx, func(q store.Querier) (err error) { out, err = store.CreditNotesForPayment(ctx, q, paymentID); return }); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// refundSucceeded creates a refund and marks it succeeded the way the refund
// job will, so the credit note hook runs.
func (e *env) refundSucceeded(p store.Payment, amount int64) store.Refund {
	e.t.Helper()
	r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: core.Scope{AppID: p.AppID, Mode: p.Mode}, Actor: audit.System, InitiatedBy: "api",
		PaymentID: p.ID, Amount: amount, Reason: "customer request"})
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

func TestInvoiceListFilters(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha, ent1 := e.taxApp("alpha", supplierGSTIN)
		beta, ent2 := e.taxApp("beta", gstin(t, "29AAPFU0939F1Z"))
		p1 := e.pay(alpha, 118000, "INR", intraB2C)
		p2 := e.pay(alpha, 118000, "INR", noState) // State unknown: place of supply defaulted
		p3 := e.pay(alpha, 1200, "USD", usBuyer)   // no FX rate yet
		p4 := e.pay(beta, 59000, "INR", interB2C)  // other entity
		e.refundSucceeded(p1, 10000)
		e.runJobs()
		c := e.roleClient("viewer")
		num := func(p store.Payment) string { return e.invoicesOf(p.ID)[0].Number }
		link := func(p store.Payment) string { return `href="/dashboard/invoices/` + e.invoicesOf(p.ID)[0].ID + `"` }

		// The default view is the invoices of the session's mode, newest first (2 per page here).
		e.setMode(c, "live")
		body := e.pageOK(c, "/dashboard/invoices")
		has(t, body, link(p4), link(p3))
		lacks(t, body, link(p2), link(p1))
		more := strings.ReplaceAll(regexpFind(t, `href="([^"]*after=[^"]*)"`, body), "&amp;", "&")
		has(t, e.pageOK(c, more), link(p2), link(p1))

		type tc struct {
			name, query string
			want, not   []store.Payment
		}
		for _, c2 := range []tc{
			{"entity", "?entity=" + ent2.ID, []store.Payment{p4}, []store.Payment{p1, p2, p3}},
			{"app", "?app=" + alpha.ID, []store.Payment{p1, p2, p3}, []store.Payment{p4}},
			{"month", "?month=2027-01", []store.Payment{p1, p2, p3, p4}, nil},
			{"other month", "?month=2026-12", nil, []store.Payment{p1, p2, p3, p4}},
			{"fx pending", "?flag=fx_pending", []store.Payment{p3}, []store.Payment{p1, p2, p4}},
			{"pos defaulted", "?flag=pos_defaulted", []store.Payment{p2}, []store.Payment{p1, p3, p4}},
		} {
			q := c2.query
			// the page size is 2: ask for every page and join them
			var all strings.Builder
			next := "/dashboard/invoices" + q
			for i := 0; next != "" && i < 5; i++ {
				b := e.pageOK(c, next)
				all.WriteString(b)
				next = ""
				if m := regexp.MustCompile(`href="([^"]*after=[^"]*)"`).FindStringSubmatch(b); m != nil {
					next = strings.ReplaceAll(m[1], "&amp;", "&")
				}
			}
			for _, p := range c2.want {
				if !strings.Contains(all.String(), link(p)) {
					t.Errorf("%s: missing %s", c2.name, num(p))
				}
			}
			for _, p := range c2.not {
				if strings.Contains(all.String(), link(p)) {
					t.Errorf("%s: unexpected %s", c2.name, num(p))
				}
			}
		}
		_ = ent1

		// credit notes
		body = e.pageOK(c, "/dashboard/invoices?type=credit_note")
		cn := e.creditNotesOf(p1.ID)[0]
		has(t, body, `href="/dashboard/credit-notes/`+cn.ID+`"`, cn.Number)
		lacks(t, body, link(p1))

		// the credit-note cutoff flag lives on refunds
		r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: core.Scope{AppID: alpha.ID, Mode: "live"}, Actor: audit.System, InitiatedBy: "api", PaymentID: p2.ID, Amount: 1000})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Write(ctx, func(tx store.Tx) error {
			return store.AddRefundFlag(ctx, tx, r.ID, invoice.FlagCutoffPassed, e.clk.Now())
		})
		body = e.pageOK(c, "/dashboard/invoices?flag=credit_note_cutoff_passed")
		has(t, body, r.ID, "/dashboard/payments/"+p2.ID)

		for _, q := range []string{"?month=jan", "?type=receipt", "?flag=bogus", "?entity=te_nope"} {
			if rr, _ := e.get(c, "/dashboard/invoices"+q); rr.StatusCode != 400 {
				t.Errorf("%s: %d", q, rr.StatusCode)
			}
		}
	})
}

func TestInvoiceDetailAndPDF(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		p := e.pay(app, 118000, "INR", intraB2C)
		inv := e.invoicesOf(p.ID)[0]
		c := e.roleClient("viewer")
		e.setMode(c, "live")

		// the PDF is not rendered until the job ran
		if r, _ := e.get(c, "/dashboard/invoices/"+inv.ID+"/pdf"); r.StatusCode != 404 {
			t.Errorf("pdf before render: %d", r.StatusCode)
		}
		e.runJobs()
		body := e.pageOK(c, "/dashboard/invoices/"+inv.ID)
		has(t, body, inv.Number, "Asha", "27AAPFU0939F1ZV", "998315", "1180.00 INR", "b2c", "Legal alpha", p.ID,
			`href="/dashboard/invoices/`+inv.ID+`/pdf"`, "CGST", "SGST")

		r, pdf := e.get(c, "/dashboard/invoices/"+inv.ID+"/pdf")
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/pdf" || !strings.HasPrefix(pdf, "%PDF-") ||
			!strings.Contains(r.Header.Get("Content-Disposition"), strings.ReplaceAll(inv.Number, "/", "-")+".pdf") {
			t.Fatalf("pdf: %d %q %q", r.StatusCode, r.Header.Get("Content-Type"), r.Header.Get("Content-Disposition"))
		}
		// dashboard authentication is required
		if r, _ := e.get(e.client(), "/dashboard/invoices/"+inv.ID+"/pdf"); r.StatusCode != 303 {
			t.Errorf("anonymous pdf: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/invoices/inv_nope"); r.StatusCode != 404 {
			t.Errorf("unknown invoice: %d", r.StatusCode)
		}

		// credit notes
		e.refundSucceeded(p, 10000)
		e.runJobs()
		cn := e.creditNotesOf(p.ID)[0]
		body = e.pageOK(c, "/dashboard/invoices/"+inv.ID)
		has(t, body, cn.Number, `href="/dashboard/credit-notes/`+cn.ID+`"`)
		body = e.pageOK(c, "/dashboard/credit-notes/"+cn.ID)
		has(t, body, cn.Number, inv.Number, "100.00 INR", `href="/dashboard/credit-notes/`+cn.ID+`/pdf"`, "customer request")
		r, pdf = e.get(c, "/dashboard/credit-notes/"+cn.ID+"/pdf")
		if r.StatusCode != 200 || !strings.HasPrefix(pdf, "%PDF-") {
			t.Fatalf("credit note pdf: %d", r.StatusCode)
		}
		if r, _ := e.get(c, "/dashboard/credit-notes/crn_nope"); r.StatusCode != 404 {
			t.Errorf("unknown credit note: %d", r.StatusCode)
		}
	})
}

func TestResendFromDashboard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		p := e.pay(app, 118000, "INR", intraB2C)
		pNoMail := e.pay(app, 118000, "INR", noEmail)
		e.runJobs()
		inv, invNoMail := e.invoicesOf(p.ID)[0], e.invoicesOf(pNoMail.ID)[0]
		page := "/dashboard/invoices/" + inv.ID

		viewer := e.roleClient("viewer")
		vb := e.pageOK(viewer, page)
		lacks(t, vb, `action="`+page+`/resend"`)
		vt := csrfOf(t, vb)
		if r, _ := e.post(viewer, page+"/resend", url.Values{"csrf": {vt}}); r.StatusCode != 403 {
			t.Fatalf("viewer: %d", r.StatusCode)
		}

		op := e.roleClient("operator")
		body := e.pageOK(op, page)
		has(t, body, `action="`+page+`/resend"`)
		tok := csrfOf(t, body)
		if r, _ := e.post(op, page+"/resend", url.Values{"csrf": {tok}}); r.StatusCode != 303 || r.Header.Get("Location") != page {
			t.Fatalf("resend: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		if e.mail.count() != 1 || e.mail.sent[0].To[0] != "asha@example.com" || len(e.mail.sent[0].Attachments) != 1 {
			t.Fatalf("emails %+v", e.mail.sent)
		}
		has(t, e.pageOK(op, page), "asha@example.com", "sent") // the send history
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "invoice.resend", 5)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_operator" || aud[0].Target != inv.ID {
			t.Fatalf("audit %+v", aud)
		}

		// credit notes can be resent too
		e.refundSucceeded(p, 10000)
		e.runJobs()
		cn := e.creditNotesOf(p.ID)[0]
		if r, _ := e.post(op, "/dashboard/credit-notes/"+cn.ID+"/resend", url.Values{"csrf": {tok}}); r.StatusCode != 303 || e.mail.count() != 2 {
			t.Fatalf("credit note resend: %d (emails %d)", r.StatusCode, e.mail.count())
		}

		// problems are shown, not swallowed
		if r, b := e.post(op, "/dashboard/invoices/"+invNoMail.ID+"/resend", url.Values{"csrf": {tok}}); r.StatusCode != 400 || !strings.Contains(b, "no email") {
			t.Errorf("no email: %d", r.StatusCode)
		}
		e.mail.mu.Lock()
		e.mail.err = errors.New("smtp down")
		e.mail.mu.Unlock()
		if r, b := e.post(op, page+"/resend", url.Values{"csrf": {tok}}); r.StatusCode != 502 || !strings.Contains(b, "smtp down") {
			t.Errorf("mail failure: %d", r.StatusCode)
		}
		if r, _ := e.post(op, "/dashboard/invoices/inv_nope/resend", url.Values{"csrf": {tok}}); r.StatusCode != 404 {
			t.Errorf("unknown: %d", r.StatusCode)
		}
		if r, _ := e.post(op, page+"/resend", url.Values{}); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}
	})
}

func TestReissueFromDashboard(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		p := e.pay(app, 118000, "INR", intraB2C)
		e.runJobs()
		old := e.invoicesOf(p.ID)[0]
		page := "/dashboard/invoices/" + old.ID
		buyer := gstin(t, "29ABCDE1234F1Z")
		form := func(tok string) url.Values {
			return url.Values{"legal_name": {"Acme Traders Pvt Ltd"}, "gstin": {buyer}, "state_code": {"29"}, "line1": {"1 Brigade Rd"}, "city": {"Bengaluru"},
				"postal_code": {"560001"}, "country": {"IN"}, "reason": {"Customer supplied a GSTIN"}, "csrf": {tok}}
		}

		// only admins reissue: the form is hidden from operators and viewers, and refused
		for _, role := range []string{"viewer", "operator"} {
			c := e.roleClient(role)
			b := e.pageOK(c, page)
			lacks(t, b, `action="`+page+`/reissue"`)
			if r, _ := e.post(c, page+"/reissue", form(csrfOf(t, b))); r.StatusCode != 403 {
				t.Fatalf("%s: %d", role, r.StatusCode)
			}
		}
		if len(e.invoicesOf(p.ID)) != 1 {
			t.Fatal("a refused reissue created an invoice")
		}

		admin := e.roleClient("admin")
		body := e.pageOK(admin, page)
		has(t, body, `action="`+page+`/reissue"`)
		tok := csrfOf(t, body)
		bad := form(tok)
		bad.Set("gstin", "29ABCDE1234F1Z0")
		if r, b := e.post(admin, page+"/reissue", bad); r.StatusCode != 400 || !strings.Contains(b, "billing details") {
			t.Fatalf("bad gstin: %d", r.StatusCode)
		}
		r, _ := e.post(admin, page+"/reissue", form(tok))
		invs := e.invoicesOf(p.ID)
		if r.StatusCode != 303 || len(invs) != 2 || r.Header.Get("Location") != "/dashboard/invoices/"+invs[1].ID {
			t.Fatalf("reissue: %d %s (invoices %d)", r.StatusCode, r.Header.Get("Location"), len(invs))
		}
		cns := e.creditNotesOf(p.ID)
		if len(cns) != 1 || cns[0].InvoiceID != old.ID || cns[0].Total != old.Total || invs[1].IGST == 0 || invs[1].CGST != 0 {
			t.Fatalf("credit notes %+v, new invoice %+v", cns, invs[1])
		}
		nb := e.pageOK(admin, r.Header.Get("Location"))
		has(t, nb, invs[1].Number, "Acme Traders Pvt Ltd", buyer, "reissued")
		has(t, e.pageOK(admin, page), cns[0].Number) // the old invoice shows its credit note
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "invoice.reissue", 5)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_admin" || aud[0].Target != old.ID {
			t.Fatalf("audit %+v", aud)
		}
		// the spent invoice cannot be reissued again
		if r, b := e.post(admin, page+"/reissue", form(tok)); r.StatusCode != 400 || !strings.Contains(b, "credit notes") {
			t.Errorf("second reissue: %d", r.StatusCode)
		}
		if r, _ := e.post(admin, "/dashboard/invoices/inv_nope/reissue", form(tok)); r.StatusCode != 404 {
			t.Errorf("unknown: %d", r.StatusCode)
		}
		_ = http.StatusOK
	})
}

func TestInvoicesPageQFilter(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		alpha, _ := e.taxApp("alpha", supplierGSTIN)
		p1 := e.pay(alpha, 118000, "INR", intraB2C) // Asha
		p2 := e.pay(alpha, 118000, "INR", noState)  // Nina
		p3 := e.pay(alpha, 1200, "USD", usBuyer)    // Bob
		e.runJobs()
		c := e.roleClient("viewer")
		e.setMode(c, "live")
		link := func(p store.Payment) string { return `href="/dashboard/invoices/` + e.invoicesOf(p.ID)[0].ID + `"` }
		for _, tc := range []struct {
			q         string
			want, not []store.Payment
		}{
			{"asha", []store.Payment{p1}, []store.Payment{p2, p3}},
			{"Bob", []store.Payment{p3}, []store.Payment{p1, p2}},
			{e.invoicesOf(p2.ID)[0].Number, []store.Payment{p2}, []store.Payment{p1, p3}},
			{"%", nil, []store.Payment{p1, p2, p3}},
			{"nobody", nil, []store.Payment{p1, p2, p3}},
		} {
			body := e.pageOK(c, "/dashboard/invoices?q="+url.QueryEscape(tc.q))
			for _, p := range tc.want {
				has(t, body, link(p))
			}
			for _, p := range tc.not {
				lacks(t, body, link(p))
			}
			has(t, body, `name="q"`, `placeholder="Invoice number or buyer"`)
		}
		// the search survives paging
		body := e.pageOK(c, "/dashboard/invoices?q=INV")
		has(t, body, `q=INV`)
		if m := regexp.MustCompile(`href="([^"]*after=[^"]*)"`).FindStringSubmatch(body); m == nil || !strings.Contains(m[1], "q=INV") {
			t.Errorf("next link drops q: %v", m)
		}
	})
}
