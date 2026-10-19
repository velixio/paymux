package dashboard

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/invoice/einvoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestCreditNotePageOffersItsEInvoiceForB2BOnly(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		app, _ := e.taxApp("alpha", supplierGSTIN)
		pB2B := e.pay(app, 118000, "INR", b2bBuyer(t))
		pB2C := e.pay(app, 118000, "INR", intraB2C)
		e.refundSucceeded(pB2B, 59000)
		e.refundSucceeded(pB2C, 59000)
		e.runJobs()
		cnB2B, cnB2C := e.creditNotesOf(pB2B.ID)[0], e.creditNotesOf(pB2C.ID)[0]
		viewer := e.roleClient("viewer")

		page := "/dashboard/credit-notes/" + cnB2B.ID
		has(t, e.pageOK(viewer, page), "Download e-invoice JSON", `href="`+page+`/einvoice.json"`)
		lacks(t, e.pageOK(viewer, "/dashboard/credit-notes/"+cnB2C.ID), "e-invoice", "/einvoice.json")

		r, body := e.get(viewer, page+"/einvoice.json")
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") ||
			!strings.Contains(r.Header.Get("Content-Disposition"), `attachment; filename="einvoice-credit-note-`) {
			t.Fatalf("download: %d %v", r.StatusCode, r.Header)
		}
		if err := einvoice.Validate([]byte(body)); err != nil {
			t.Fatalf("not a valid e-invoice: %v\n%s", err, body)
		}
		has(t, body, `"Typ": "CRN"`, `"No": "`+cnB2B.Number+`"`, `"PrecDocDtls"`)
		if r, _ := e.get(viewer, "/dashboard/credit-notes/"+cnB2C.ID+"/einvoice.json"); r.StatusCode != 400 {
			t.Errorf("a B2C credit note has none: %d", r.StatusCode)
		}
		if r, _ := e.get(viewer, "/dashboard/credit-notes/crn_nope/einvoice.json"); r.StatusCode != 404 {
			t.Errorf("unknown credit note: %d", r.StatusCode)
		}
	})
}
