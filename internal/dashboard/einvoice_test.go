package dashboard

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/invoice/einvoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/internal/tax"
)

var irnHex = strings.Repeat("0123456789abcdef", 4)

func signedQR(irn, docNo, sellerGSTIN string) string {
	data, _ := json.Marshal(map[string]any{"SellerGstin": sellerGSTIN, "DocNo": docNo, "Irn": irn})
	payload, _ := json.Marshal(map[string]any{"data": string(data), "iss": "NIC"})
	b := func(s []byte) string { return base64.RawURLEncoding.EncodeToString(s) }
	return b([]byte(`{"alg":"RS256"}`)) + "." + b(payload) + "." + b([]byte("signature"))
}

func b2bBuyer(t *testing.T) *core.CustomerInput {
	return &core.CustomerInput{Name: "Accounts", Email: "accounts@example.com", Billing: store.Billing{GSTIN: gstin(t, "29ABCDE1234F1Z"), LegalName: "Acme Traders LLP",
		StateCode: "29", Country: "IN", Line1: "5 Residency Road", City: "Bengaluru", PostalCode: "560025"}}
}

func TestInvoicePageOffersTheEInvoiceForB2BOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		b2b := e.invoicesOf(e.pay(app, 118000, "INR", b2bBuyer(t)).ID)[0]
		b2c := e.invoicesOf(e.pay(app, 118000, "INR", intraB2C).ID)[0]
		e.runJobs()

		op, viewer := e.roleClient("operator"), e.roleClient("viewer")
		page := "/dashboard/invoices/" + b2b.ID
		ob := e.pageOK(op, page)
		has(t, ob, "Download e-invoice JSON", `href="`+page+`/einvoice.json"`, `action="`+page+`/einvoice"`, `name="irn"`, `name="ack_no"`, `name="ack_date"`, `name="signed_qr"`)
		vb := e.pageOK(viewer, page)
		has(t, vb, "Download e-invoice JSON")
		lacks(t, vb, `action="`+page+`/einvoice"`)
		lacks(t, e.pageOK(op, "/dashboard/invoices/"+b2c.ID), "e-invoice", `/einvoice`)
	})
}

func TestDownloadEInvoiceJSON(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		inv := e.invoicesOf(e.pay(app, 118000, "INR", b2bBuyer(t)).ID)[0]
		b2c := e.invoicesOf(e.pay(app, 118000, "INR", intraB2C).ID)[0]
		viewer := e.roleClient("viewer")

		r, body := e.get(viewer, "/dashboard/invoices/"+inv.ID+"/einvoice.json")
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") ||
			!strings.Contains(r.Header.Get("Content-Disposition"), `attachment; filename="einvoice-INV-2627-000001.json"`) {
			t.Fatalf("download: %d %v", r.StatusCode, r.Header)
		}
		if err := einvoice.Validate([]byte(body)); err != nil {
			t.Fatalf("not a valid e-invoice: %v\n%s", err, body)
		}
		has(t, body, `"SupTyp": "B2B"`, `"No": "`+inv.Number+`"`, supplierGSTIN)
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("cache %q", r.Header.Get("Cache-Control"))
		}

		if r, _ := e.get(viewer, "/dashboard/invoices/"+b2c.ID+"/einvoice.json"); r.StatusCode != 400 {
			t.Errorf("a B2C invoice has none: %d", r.StatusCode)
		}
		if r, _ := e.get(viewer, "/dashboard/invoices/inv_nope/einvoice.json"); r.StatusCode != 404 {
			t.Errorf("unknown invoice: %d", r.StatusCode)
		}
		e.otherOrg()
		_ = db.Write(ctx, func(tx store.Tx) error {
			foreign := inv
			foreign.ID, foreign.OrgID, foreign.Number, foreign.AccessTokenHash = "inv_foreign", "org_other", "INV/2627/900001", "hx"
			foreign.AppID, foreign.TaxEntityID, foreign.PaymentID, foreign.AttemptID = "app_foreign", inv.TaxEntityID, "pay_foreign", "att_foreign"
			return store.InsertInvoice(ctx, tx, foreign)
		})
		if r, _ := e.get(viewer, "/dashboard/invoices/inv_foreign/einvoice.json"); r.StatusCode != 404 {
			t.Errorf("another org's invoice: %d", r.StatusCode)
		}
	})
}

func TestEInvoiceProblemsAreExplainedOnThePage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		// A supplier address without a PIN code cannot be an e-invoice seller.
		inv := e.invoicesOf(e.pay(app, 118000, "INR", b2bBuyer(t)).ID)[0]
		// The invoice keeps the address it was issued with, which has the PIN: rewrite it to make the point.
		_ = db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE invoices SET supplier = REPLACE(supplier, 'Mumbai 400001', 'Mumbai') WHERE id = ?`, inv.ID)
			return err
		})
		viewer := e.roleClient("viewer")
		r, body := e.get(viewer, "/dashboard/invoices/"+inv.ID+"/einvoice.json")
		if r.StatusCode != 422 || !strings.Contains(body, "PIN") || !strings.Contains(body, "seller") || !strings.Contains(body, `class="error"`) {
			t.Fatalf("problems: %d\n%s", r.StatusCode, body)
		}
		if strings.Contains(r.Header.Get("Content-Type"), "json") {
			t.Error("the problem page is HTML, not a broken download")
		}
	})
}

func TestOperatorRecordsThePortalsAnswer(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		inv := e.invoicesOf(e.pay(app, 118000, "INR", b2bBuyer(t)).ID)[0]
		op, viewer := e.roleClient("operator"), e.roleClient("viewer")
		page := "/dashboard/invoices/" + inv.ID
		tok := csrfOf(t, e.pageOK(op, page))
		form := func() url.Values {
			return url.Values{"irn": {irnHex}, "ack_no": {"112610012345678"}, "ack_date": {"2027-01-15 14:00:00"}, "signed_qr": {signedQR(irnHex, inv.Number, supplierGSTIN)}, "csrf": {tok}}
		}

		vtok := csrfOf(t, e.pageOK(viewer, page))
		vf := form()
		vf.Set("csrf", vtok)
		if r, _ := e.post(viewer, page+"/einvoice", vf); r.StatusCode != 403 {
			t.Errorf("viewer: %d", r.StatusCode)
		}
		nf := form()
		nf.Del("csrf")
		if r, _ := e.post(op, page+"/einvoice", nf); r.StatusCode != 403 {
			t.Errorf("no csrf: %d", r.StatusCode)
		}

		bad := form()
		bad.Set("irn", "short")
		r, body := e.post(op, page+"/einvoice", bad)
		if r.StatusCode != 400 || !strings.Contains(body, "IRN") || !strings.Contains(body, `class="error"`) || !strings.Contains(body, "112610012345678") {
			t.Fatalf("bad input should be explained and the form kept: %d\n%s", r.StatusCode, body)
		}

		if r, _ := e.post(op, page+"/einvoice", form()); r.StatusCode != 303 || r.Header.Get("Location") != page {
			t.Fatalf("record: %d %s", r.StatusCode, r.Header.Get("Location"))
		}
		got := e.invoicesOf(inv.PaymentID)[0]
		if got.IRN != irnHex || got.AckNo != "112610012345678" || got.SignedQR == "" {
			t.Fatalf("stored %+v", got)
		}
		after := e.pageOK(op, page)
		has(t, after, irnHex, "112610012345678", "2027-01-15 14:00:00")
		lacks(t, after, got.SignedQR) // the long token is not dumped on the page
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "invoice.einvoice_record", 5)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_operator" || aud[0].Target != inv.ID {
			t.Errorf("audit %+v", aud)
		}
		if r, _ := e.post(op, "/dashboard/invoices/inv_nope/einvoice", form()); r.StatusCode != 404 {
			t.Errorf("unknown invoice: %d", r.StatusCode)
		}
	})
}

func TestReportsPageExportsAMonthOfEInvoices(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, ent := e.taxApp("alpha", supplierGSTIN)
		ready := e.invoicesOf(e.pay(app, 118000, "INR", b2bBuyer(t)).ID)[0]
		e.pay(app, 118000, "INR", intraB2C)
		noPin := &core.CustomerInput{Name: "X", Email: "x@example.com", Billing: store.Billing{GSTIN: gstin(t, "29ABCDE1234F1Z"), LegalName: "No Pin Ltd", StateCode: "29", Country: "IN", Line1: "1 Road", City: "Pune"}}
		problem := e.invoicesOf(e.pay(app, 118000, "INR", noPin).ID)[0]
		done := e.invoicesOf(e.pay(app, 118000, "INR", b2bBuyer(t)).ID)[0]
		if _, err := e.d.o.Invoice.RecordEInvoice(ctx, audit.System, done.ID, invoice.EInvoiceRecord{IRN: strings.Repeat("a1", 32), AckNo: "1",
			AckDate: "2027-01-15 14:00:00", SignedQR: signedQR(strings.Repeat("a1", 32), done.Number, supplierGSTIN)}); err != nil {
			t.Fatal(err)
		}
		month := e.clk.Now().In(tax.IST).Format("2006-01")

		viewer := e.roleClient("viewer")
		q := url.Values{"entity": {ent.ID}, "month": {month}}.Encode()
		body := e.pageOK(viewer, "/dashboard/reports?"+q)
		has(t, body, "Export month as e-invoice JSON", "/dashboard/reports/einvoice.json?"+strings.ReplaceAll(q, "&", "&amp;"), ready.Number, problem.Number, done.Number,
			"ready", "PIN", "registered")
		row := func(n string) string {
			for _, seg := range strings.Split(body, "<tr") {
				if strings.Contains(seg, n) {
					return seg
				}
			}
			t.Fatalf("no row for %s", n)
			return ""
		}
		has(t, row(ready.Number), "ready")
		has(t, row(problem.Number), "problem")
		has(t, row(done.Number), "done")

		r, js := e.get(viewer, "/dashboard/reports/einvoice.json?"+q)
		if r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Disposition"), `filename="einvoice-`+ent.ID+`-`+month+`.json"`) {
			t.Fatalf("download: %d %v", r.StatusCode, r.Header)
		}
		if err := einvoice.ValidateBatch([]byte(js)); err != nil {
			t.Fatal(err)
		}
		var docs []map[string]any
		_ = json.Unmarshal([]byte(js), &docs)
		if len(docs) != 1 || docs[0]["DocDtls"].(map[string]any)["No"] != ready.Number {
			t.Fatalf("only the ready invoice goes in the default export: %s", js)
		}
		_, all := e.get(viewer, "/dashboard/reports/einvoice.json?"+q+"&all=1")
		_ = json.Unmarshal([]byte(all), &docs)
		if len(docs) != 2 {
			t.Errorf("with the registered ones: %d documents", len(docs))
		}

		for _, bad := range []string{"entity=" + ent.ID + "&month=October", "entity=te_nope&month=" + month, "month=" + month} {
			if r, _ := e.get(viewer, "/dashboard/reports/einvoice.json?"+bad); r.StatusCode != 400 {
				t.Errorf("%s: %d", bad, r.StatusCode)
			}
		}
	})
}
