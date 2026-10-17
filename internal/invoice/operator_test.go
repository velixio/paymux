package invoice_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

var operator = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

func noEmail() *core.CustomerInput {
	return &core.CustomerInput{Name: "Nobody", Billing: store.Billing{StateCode: "27", Country: "IN"}}
}

func auditRows(t *testing.T, e *env, action string) []store.AuditRow {
	t.Helper()
	var rows []store.AuditRow
	if err := e.db.Read(ctx, func(q store.Querier) (err error) {
		rows, err = store.ListAudit(ctx, q, store.DefaultOrgID, action, 20)
		return
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestUpdateTaxEntity(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		in := invoice.TaxEntityInput{LegalName: "Voice Studio Pvt Ltd", TradeName: "VS Pay", GSTIN: supplierGSTIN, Address: "New address, Mumbai",
			LUTNumber: "AD270327000009Z", LUTFrom: time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), LUTTo: time.Date(2028, 3, 31, 0, 0, 0, 0, time.UTC),
			InvoicePrefix: "VS", CreditNotePrefix: "VSC", SignatoryName: "B. Signer", FooterNote: "Pay on time", SupportEmail: "ops@example.com",
			LogoBlobID: "blob_logo"}
		got, err := e.inv.UpdateTaxEntity(ctx, operator, "te_1", in)
		if err != nil {
			t.Fatal(err)
		}
		if got.TradeName != "VS Pay" || got.Address != "New address, Mumbai" || got.InvoicePrefix != "VS" || got.LogoBlobID != "blob_logo" ||
			got.LUTNumber != "AD270327000009Z" || got.StateCode != "27" || got.PAN != "AAPFU0939F" {
			t.Fatalf("%+v", got)
		}
		saved, _ := e.inv.FindTaxEntity(ctx, "te_1")
		if saved.SignatoryName != "B. Signer" || !saved.UpdatedAt.Equal(e.clock()) {
			t.Fatalf("saved %+v", saved)
		}
		if rows := auditRows(t, e, "tax_entity.update"); len(rows) != 1 || rows[0].ActorID != "usr_op" || rows[0].Target != "te_1" {
			t.Fatalf("audit %+v", rows)
		}

		// an empty logo id keeps the logo; RemoveLogo clears it; clearing the LUT is allowed
		in.LogoBlobID, in.LUTNumber, in.LUTFrom, in.LUTTo = "", "", time.Time{}, time.Time{}
		got, err = e.inv.UpdateTaxEntity(ctx, operator, "te_1", in)
		if err != nil || got.LogoBlobID != "blob_logo" || got.LUTNumber != "" {
			t.Fatalf("keep logo / clear LUT: %+v %v", got, err)
		}
		in.RemoveLogo = true
		if got, _ = e.inv.UpdateTaxEntity(ctx, operator, "te_1", in); got.LogoBlobID != "" {
			t.Fatalf("logo not removed: %+v", got)
		}

		bad := map[string]func(*invoice.TaxEntityInput){
			"gstin change":  func(i *invoice.TaxEntityInput) { i.GSTIN = validGSTIN(t, "29ABCDE1234F1Z") },
			"no legal name": func(i *invoice.TaxEntityInput) { i.LegalName = " " },
			"bad prefix":    func(i *invoice.TaxEntityInput) { i.InvoicePrefix = "toolong1" },
			"half a LUT":    func(i *invoice.TaxEntityInput) { i.LUTNumber = "AD1" },
			"reversed LUT": func(i *invoice.TaxEntityInput) {
				i.LUTNumber, i.LUTFrom, i.LUTTo = "AD1", time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
			},
			"bad gstin":      func(i *invoice.TaxEntityInput) { i.GSTIN = "27AAPFU0939F1Z0" },
			"state mismatch": func(i *invoice.TaxEntityInput) { i.StateCode = "29" },
		}
		for name, mut := range bad {
			c := in
			c.RemoveLogo = false
			mut(&c)
			if _, err := e.inv.UpdateTaxEntity(ctx, operator, "te_1", c); !errors.Is(err, invoice.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := e.inv.UpdateTaxEntity(ctx, operator, "te_missing", in); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("unknown entity: %v", err)
		}
	})
}

func TestResendDocument(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		invc := only(t, e, p.ID)
		if e.mailer.count() != 0 {
			t.Fatal("setup: delivery=app must not email")
		}

		// a resend is sent whatever the delivery mode, and every time it is asked for
		to, err := e.inv.ResendDocument(ctx, operator, "invoice", invc.ID)
		if err != nil || to != "asha@example.com" {
			t.Fatalf("resend: %q %v", to, err)
		}
		if _, err := e.inv.ResendDocument(ctx, operator, "invoice", invc.ID); err != nil {
			t.Fatal(err)
		}
		if e.mailer.count() != 2 {
			t.Fatalf("emails=%d", e.mailer.count())
		}
		msg := e.mailer.sent[0]
		if msg.To[0] != "asha@example.com" || !strings.Contains(msg.Subject, invc.Number) || len(msg.Attachments) != 1 ||
			!strings.HasPrefix(string(msg.Attachments[0].Data), "%PDF-") || !strings.HasSuffix(msg.Attachments[0].Filename, ".pdf") {
			t.Fatalf("message %+v", msg)
		}
		var sends []store.DocumentSend
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			sends, err = store.DocumentSends(ctx, q, "invoice", invc.ID)
			return
		})
		if len(sends) != 2 || sends[0].Status != "sent" || sends[0].Recipient != "asha@example.com" {
			t.Fatalf("sends %+v", sends)
		}
		if rows := auditRows(t, e, "invoice.resend"); len(rows) != 2 || rows[0].ActorID != "usr_op" || rows[0].Target != invc.ID {
			t.Fatalf("audit %+v", rows)
		}

		// credit notes too
		e.refund(p, 10000)
		e.runJobs()
		cn := e.creditNotes(p.ID)[0]
		if _, err := e.inv.ResendDocument(ctx, operator, "credit_note", cn.ID); err != nil || e.mailer.count() != 3 {
			t.Fatalf("credit note resend: %v (emails %d)", err, e.mailer.count())
		}

		// failures are recorded and returned
		e.mailer.err = errors.New("smtp down")
		if _, err := e.inv.ResendDocument(ctx, operator, "invoice", invc.ID); err == nil || !strings.Contains(err.Error(), "smtp down") {
			t.Fatalf("send failure: %v", err)
		}
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			sends, err = store.DocumentSends(ctx, q, "invoice", invc.ID)
			return
		})
		if sends[len(sends)-1].Status != "failed" {
			t.Fatalf("failed send not recorded: %+v", sends)
		}
		e.mailer.err = nil

		for name, c := range map[string]struct{ kind, id string }{
			"bad kind": {"receipt", invc.ID}, "unknown invoice": {"invoice", "inv_missing"}, "unknown credit note": {"credit_note", "crn_missing"},
		} {
			if _, err := e.inv.ResendDocument(ctx, operator, c.kind, c.id); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})
}

func TestResendNeedsAnEmailAndAKnownRate(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", noEmail(), "pa_1")
		e.runJobs()
		if _, err := e.inv.ResendDocument(ctx, operator, "invoice", only(t, e, p.ID).ID); !errors.Is(err, invoice.ErrInvalid) {
			t.Fatalf("no email: %v", err)
		}
		// a foreign-currency invoice without its INR rate cannot be rendered yet
		pu, _ := e.pay(1200, "USD", usBuyer, "pa_1")
		e.runJobs()
		if _, err := e.inv.ResendDocument(ctx, operator, "invoice", only(t, e, pu.ID).ID); !errors.Is(err, invoice.ErrInvalid) {
			t.Fatalf("fx pending: %v", err)
		}
		if e.mailer.count() != 0 {
			t.Fatal("sent despite the error")
		}
	})
}

func TestReissueInvoiceWithCorrectedBilling(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		old := only(t, e, p.ID)
		if old.CGST == 0 || old.IGST != 0 {
			t.Fatalf("setup: expected an intra-state invoice, got %+v", old)
		}

		buyerGSTIN := validGSTIN(t, "29ABCDE1234F1Z")
		res, err := e.inv.ReissueInvoice(ctx, operator, old.ID, store.Billing{LegalName: "Acme Traders Pvt Ltd", GSTIN: buyerGSTIN, StateCode: "29",
			Line1: "1 Brigade Rd", City: "Bengaluru", PostalCode: "560001", Country: "IN"}, "Customer supplied a GSTIN")
		if err != nil {
			t.Fatal(err)
		}
		// a full credit note against the old invoice
		cn := res.CreditNote
		if cn.InvoiceID != old.ID || cn.Total != old.Total || cn.Taxable != old.Taxable || cn.CGST != old.CGST || cn.SGST != old.SGST ||
			cn.RefundID != "" || !strings.Contains(cn.Reason, "Customer supplied a GSTIN") || cn.Currency != "INR" {
			t.Fatalf("credit note %+v vs invoice %+v", cn, old)
		}
		// and a new invoice with the corrected classification
		ni := res.Invoice
		if ni.ID == old.ID || ni.Number == old.Number || ni.Total != old.Total || ni.IGST == 0 || ni.CGST != 0 || !ni.InterState ||
			ni.SupplyType != "b2b" || ni.PaymentID != p.ID || !ni.HasFlag(invoice.FlagReissued) {
			t.Fatalf("new invoice %+v", ni)
		}
		if !strings.Contains(string(ni.Recipient), "Acme Traders Pvt Ltd") || !strings.Contains(string(ni.Recipient), buyerGSTIN) {
			t.Fatalf("recipient snapshot %s", ni.Recipient)
		}
		// the customer's billing was corrected for next time
		var c store.Customer
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			pp, _ := store.GetPayment(ctx, q, p.ID)
			c, err = store.GetCustomer(ctx, q, pp.CustomerID)
			return
		})
		if c.Billing.GSTIN != buyerGSTIN || c.Billing.StateCode != "29" {
			t.Fatalf("customer billing %+v", c.Billing)
		}
		// both documents are rendered, announced and audited
		e.runJobs()
		pdfOf(t, e, e.creditNotes(p.ID)[0].PDFBlobID)
		invs := e.invoices(p.ID)
		if len(invs) != 2 {
			t.Fatalf("invoices %d", len(invs))
		}
		pdfOf(t, e, invs[1].PDFBlobID)
		types := strings.Join(e.emittedTypes(), ",")
		if strings.Count(types, "invoice.issued") != 2 || strings.Count(types, "credit_note.issued") != 1 {
			t.Fatalf("events %s", types)
		}
		rows := auditRows(t, e, "invoice.reissue")
		if len(rows) != 1 || rows[0].ActorID != "usr_op" || rows[0].Target != old.ID || !strings.Contains(rows[0].After, ni.ID) || !strings.Contains(rows[0].After, cn.ID) {
			t.Fatalf("audit %+v", rows)
		}

		// a later refund credits the new invoice, not the old one
		e.refund(p, 18000)
		cns := e.creditNotes(p.ID)
		if len(cns) != 2 || cns[1].InvoiceID != ni.ID {
			t.Fatalf("refund credit note should follow the new invoice: %+v", cns)
		}

		// the old invoice is spent
		if _, err := e.inv.ReissueInvoice(ctx, operator, old.ID, store.Billing{Country: "IN"}, ""); !errors.Is(err, invoice.ErrInvalid) {
			t.Fatalf("second reissue of the old invoice: %v", err)
		}
	})
}

func TestReissueRefusals(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		old := only(t, e, p.ID)
		before := func() int { return len(e.invoices(p.ID)) + len(e.creditNotes(p.ID)) }

		// invalid corrected billing changes nothing
		if _, err := e.inv.ReissueInvoice(ctx, operator, old.ID, store.Billing{GSTIN: "27AAPFU0939F1Z0", Country: "IN"}, ""); !errors.Is(err, invoice.ErrInvalid) {
			t.Fatalf("bad gstin: %v", err)
		}
		if _, err := e.inv.ReissueInvoice(ctx, operator, "inv_missing", store.Billing{Country: "IN"}, ""); !errors.Is(err, invoice.ErrInvalid) {
			t.Fatalf("unknown invoice: %v", err)
		}
		if before() != 1 {
			t.Fatalf("a refused reissue left documents behind (%d)", before())
		}
		if rows := auditRows(t, e, "invoice.reissue"); len(rows) != 0 {
			t.Fatalf("audit for a refused reissue: %+v", rows)
		}

		// an invoice that already has credit notes (a refund) is not reissued
		e.refund(p, 10000)
		if _, err := e.inv.ReissueInvoice(ctx, operator, old.ID, store.Billing{Country: "IN", StateCode: "27"}, ""); !errors.Is(err, invoice.ErrInvalid) {
			t.Fatalf("credited invoice: %v", err)
		}

		// past the statutory credit-note cutoff nothing can be reissued
		p2, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.setNow(e.clock().AddDate(2, 0, 0))
		if _, err := e.inv.ReissueInvoice(ctx, operator, only(t, e, p2.ID).ID, store.Billing{Country: "IN", StateCode: "27"}, ""); !errors.Is(err, invoice.ErrInvalid) {
			t.Fatalf("after the cutoff: %v", err)
		}
	})
}
