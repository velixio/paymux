package invoice_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func TestRecordedEInvoiceReachesThePDFAndTheHostedPage(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /i/{id}", e.inv.ServeHosted)
		p, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		inv := only(t, e, p.ID)
		hosted, _ := urlsOf(t, e.eventsOf("invoice.issued")[0].Data)

		_, body := get(t, mux, hosted)
		if strings.Contains(body, "IRN") || strings.Contains(body, "<svg") {
			t.Error("a page before registration shows no IRN or QR code")
		}
		e.runJobs()
		plain, err := e.inv.PDF(ctx, only(t, e, p.ID).PDFBlobID)
		if err != nil {
			t.Fatal(err)
		}

		_, err = e.inv.RecordEInvoice(ctx, audit.System, inv.ID, invoice.EInvoiceRecord{IRN: irnHex, AckNo: "112610012345678",
			AckDate: "2026-10-18 10:15:30", SignedQR: fakeQR(irnHex, inv.Number, supplierGSTIN)})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs()
		with, err := e.inv.PDF(ctx, only(t, e, p.ID).PDFBlobID)
		if err != nil {
			t.Fatal(err)
		}
		if len(with) < len(plain)+1500 {
			t.Errorf("the re-rendered PDF is %d bytes against %d: the IRN block and QR code are missing", len(with), len(plain))
		}

		res, body := get(t, mux, hosted)
		if res.StatusCode != 200 {
			t.Fatalf("hosted page: %d", res.StatusCode)
		}
		for _, want := range []string{"e-Invoice", irnHex, "112610012345678", "18-10-2026", "<svg", `viewBox="0 0 `, `<path d="M`, "QR code"} {
			if !strings.Contains(body, want) {
				t.Errorf("hosted page lacks %q", want)
			}
		}
		if strings.Contains(body, "signature-bytes") || strings.Contains(body, "eyJhbGci") {
			t.Error("the signed token itself must not be printed on the page")
		}
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("csp %q", csp)
		}
	})
}

func TestHostedCreditNoteShowsNoIRN(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /i/{id}", e.inv.ServeHosted)
		p, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		inv := only(t, e, p.ID)
		if _, err := e.inv.RecordEInvoice(ctx, audit.System, inv.ID, invoice.EInvoiceRecord{IRN: irnHex, AckNo: "1",
			AckDate: "2026-10-18 10:15:30", SignedQR: fakeQR(irnHex, inv.Number, supplierGSTIN)}); err != nil {
			t.Fatal(err)
		}
		e.refund(p, 59000)
		e.runJobs()
		ch, _ := urlsOf(t, e.eventsOf("credit_note.issued")[0].Data)
		if _, body := get(t, mux, ch); strings.Contains(body, irnHex) {
			t.Error("a credit note must not show the invoice's IRN")
		}
	})
}
