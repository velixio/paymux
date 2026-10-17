package render

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/tax"
)

func signedQR() string {
	b := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return b(`{"alg":"RS256","typ":"JWT"}`) + "." + b(`{"data":"{\"Irn\":\"`+strings.Repeat("ab", 32)+`\",\"DocNo\":\"VS/2627/000042\"}","iss":"NIC"}`+strings.Repeat(" ", 900)) + "." + b(strings.Repeat("sig", 120))
}

func withEInvoice(t *testing.T) Document {
	d := interB2B(t)
	d.EInvoice = &EInvoiceInfo{IRN: strings.Repeat("ab", 32), AckNo: "112610012345678", AckDate: time.Date(2026, 11, 4, 9, 5, 7, 0, tax.IST), QR: signedQR()}
	return d
}

func TestEInvoiceBlockPrintsIRNAckAndDrawsTheQRCode(t *testing.T) {
	plain := mustRender(t, interB2B(t))
	with := mustRender(t, withEInvoice(t))
	got := text(t, with)
	wantAll(t, got, "e-Invoice", "IRN", strings.Repeat("ab", 32)[:32], "112610012345678", "04-11-2026")
	if strings.Contains(text(t, plain), "IRN") {
		t.Error("an invoice without e-invoice data must not mention an IRN")
	}
	// The QR code is vector drawing: the file grows by far more than the text.
	if len(with) < len(plain)+1500 {
		t.Errorf("PDF with the QR code is %d bytes against %d without: the code does not look drawn", len(with), len(plain))
	}
	if !bytes.Equal(with, mustRender(t, withEInvoice(t))) {
		t.Error("rendering with an e-invoice block is not deterministic")
	}
}

func TestInvoiceWithoutEInvoiceRendersAsBefore(t *testing.T) {
	a, b := mustRender(t, interB2B(t)), mustRender(t, interB2B(t))
	if !bytes.Equal(a, b) {
		t.Fatal("not deterministic")
	}
	d := interB2B(t)
	d.EInvoice = nil
	if !bytes.Equal(a, mustRender(t, d)) {
		t.Error("a nil e-invoice changes the output")
	}
}

func TestQRThatCannotBeDrawnStillPrintsTheIRN(t *testing.T) {
	d := withEInvoice(t)
	d.EInvoice.QR = strings.Repeat("A", 6000) // beyond any QR code
	got := text(t, mustRender(t, d))
	wantAll(t, got, "IRN", strings.Repeat("ab", 32)[:32], "112610012345678")
}

func TestEInvoiceOnACreditNoteIsIgnored(t *testing.T) {
	d := creditNote(t)
	d.EInvoice = &EInvoiceInfo{IRN: strings.Repeat("cd", 32), AckNo: "1", AckDate: issued, QR: signedQR()}
	if strings.Contains(text(t, mustRender(t, d)), "IRN") {
		t.Error("credit notes do not carry the invoice's IRN")
	}
}

func TestEInvoiceDoesNotBreakLongDocuments(t *testing.T) {
	d := withEInvoice(t)
	for i := 0; i < 28; i++ { // push the totals to the end of the page
		d.Lines = append(d.Lines, d.Lines[0])
	}
	got := text(t, mustRender(t, fixTotals(d)))
	wantAll(t, got, "IRN", "112610012345678")
}

// fixTotals sets Totals to the sum of the lines so validation passes.
func fixTotals(d Document) Document {
	var t tax.Amounts
	for _, l := range d.Lines {
		t = t.Add(tax.Amounts{Taxable: l.Taxable, CGST: l.CGST, SGST: l.SGST, IGST: l.IGST, Total: l.Taxable + l.CGST + l.SGST + l.IGST})
	}
	d.Totals = t
	return d
}
