package render

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/tax"
)

var issued = time.Date(2026, 11, 3, 10, 30, 0, 0, tax.IST)

func logoPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func supplier(t *testing.T) Party {
	return Party{LegalName: "Yupcha Technologies Private Limited", TradeName: "Voicestudio",
		Address: "12 MG Road, Indiranagar, Bengaluru 560038", GSTIN: "29ABCDE1234F1Z5",
		StateName: "Karnataka", StateCode: "29", Logo: logoPNG(t)}
}

func base(t *testing.T) Document {
	return Document{Kind: KindInvoice, Number: "VS/2627/000042", IssueDate: issued,
		Supplier: supplier(t), TemplateVersion: Version1,
		PaymentMethod: "UPI", PaymentRef: "pay_9X2KQ7", SignatoryName: "A. Rao",
		FooterNote: "Thank you for your business."}
}

// intra-state B2C: 18% GST on 1,000.00 -> CGST 90 + SGST 90.
func intraB2C(t *testing.T) Document {
	d := base(t)
	d.Recipient = Party{Name: "Priya Sharma", Address: "5 Park Street, Bengaluru", StateName: "Karnataka", StateCode: "29", Country: "IN"}
	d.PlaceOfSupply = "Karnataka (29)"
	d.SupplyType = tax.B2C
	d.Lines = []Line{{Description: "Voicestudio Pro — Nov 2026", SAC: "998314", Qty: 1, Unit: "service",
		Taxable: 100000, CGSTRateBP: 900, SGSTRateBP: 900, CGST: 9000, SGST: 9000}}
	d.Totals = tax.Amounts{Taxable: 100000, CGST: 9000, SGST: 9000, Total: 118000}
	return d
}

func interB2B(t *testing.T) Document {
	d := base(t)
	d.Number = "VS/2627/000043"
	d.Recipient = Party{Name: "Acme Traders LLP", Address: "44 Link Road, Mumbai", GSTIN: "27AAPFU0939F1ZV", StateName: "Maharashtra", StateCode: "27", Country: "IN"}
	d.PlaceOfSupply = "Maharashtra (27)"
	d.SupplyType = tax.B2B
	d.InterState = true
	d.Lines = []Line{{Description: "Voicestudio Team — annual", SAC: "998314", Qty: 1, Unit: "service",
		Taxable: 1000000, IGSTRateBP: 1800, IGST: 180000}}
	d.Totals = tax.Amounts{Taxable: 1000000, IGST: 180000, Total: 1180000}
	return d
}

func exportLUT(t *testing.T) Document {
	d := base(t)
	d.Number = "VS/2627/000044"
	d.Recipient = Party{Name: "Globex Inc", Address: "1 Market St, San Francisco", Country: "US", CountryName: "United States"}
	d.PlaceOfSupply = "Outside India (United States)"
	d.SupplyType = tax.ExportLUT
	d.InterState = true
	d.Lines = []Line{{Description: "Voicestudio Pro — Nov 2026", SAC: "998314", Qty: 1, Unit: "service",
		Taxable: 99900, IGSTRateBP: 0, IGST: 0}}
	d.Totals = tax.Amounts{Taxable: 99900, Total: 99900}
	d.ExportEndorsement = "Supply meant for export under Letter of Undertaking without payment of integrated tax."
	d.LUTNumber = "AD290326000123X"
	d.Currency, d.ForeignAmount, d.FXRate = "USD", 1200, "83.2500"
	return d
}

func creditNote(t *testing.T) Document {
	d := intraB2C(t)
	d.Kind = KindCreditNote
	d.Number = "VSCN/2627/000007"
	d.OriginalNumber = "VS/2627/000042"
	d.OriginalDate = time.Date(2026, 11, 3, 0, 0, 0, 0, tax.IST)
	d.Reason = "Refund of subscription charge"
	d.Lines[0].Taxable, d.Lines[0].CGST, d.Lines[0].SGST = 50000, 4500, 4500
	d.Totals = tax.Amounts{Taxable: 50000, CGST: 4500, SGST: 4500, Total: 59000}
	return d
}

func text(t *testing.T, b []byte) string { t.Helper(); return pdfText(t, b) }

func mustRender(t *testing.T, d Document) []byte {
	t.Helper()
	b, err := Render(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("%PDF-")) || !bytes.Contains(b[len(b)-32:], []byte("%%EOF")) {
		t.Fatal("output is not a PDF")
	}
	return b
}

func wantAll(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("PDF text missing %q\n--- text ---\n%s", w, got)
		}
	}
}

func TestIntraStateB2CInvoice(t *testing.T) {
	got := text(t, mustRender(t, intraB2C(t)))
	wantAll(t, got, "TAX INVOICE", "VS/2627/000042", "03-11-2026", "29ABCDE1234F1Z5", "Yupcha Technologies Private Limited",
		"Karnataka", "Priya Sharma", "998314", "service", "CGST", "SGST", "9%", "₹90.00", "₹1,000.00", "₹1,180.00",
		"Rupees One Thousand One Hundred Eighty Only", "Whether tax is payable on reverse charge: No",
		"UPI", "pay_9X2KQ7", "A. Rao", "This is a computer-generated document", "Thank you for your business.", "Voicestudio Pro — Nov 2026")
	if strings.Contains(got, "IGST") {
		t.Error("intra-state invoice shows IGST")
	}
}

func TestInterStateB2BInvoice(t *testing.T) {
	got := text(t, mustRender(t, interB2B(t)))
	wantAll(t, got, "TAX INVOICE", "VS/2627/000043", "27AAPFU0939F1ZV", "Maharashtra", "IGST", "18%", "₹1,800.00", "₹11,800.00",
		"Rupees Eleven Thousand Eight Hundred Only")
	if strings.Contains(got, "CGST") {
		t.Error("inter-state invoice shows CGST")
	}
}

func TestExportLUTInvoice(t *testing.T) {
	got := text(t, mustRender(t, exportLUT(t)))
	wantAll(t, got, "Outside India (United States)", "Globex Inc", "AD290326000123X", "without payment of integrated tax",
		"USD 12.00", "83.2500", "₹999.00")
}

func TestCreditNote(t *testing.T) {
	got := text(t, mustRender(t, creditNote(t)))
	wantAll(t, got, "CREDIT NOTE", "VSCN/2627/000007", "VS/2627/000042", "03-11-2026", "Refund of subscription charge",
		"₹500.00", "₹45.00", "₹590.00", "Rupees Five Hundred Ninety Only")
	if strings.Contains(got, "TAX INVOICE") {
		t.Error("credit note titled as tax invoice")
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	for name, d := range map[string]Document{"intra": intraB2C(t), "inter": interB2B(t), "export": exportLUT(t), "credit": creditNote(t)} {
		a, b := mustRender(t, d), mustRender(t, d)
		if !bytes.Equal(a, b) {
			t.Errorf("%s: two renders differ", name)
		}
	}
	d := intraB2C(t)
	d.Number = "VS/2627/000099"
	if bytes.Equal(mustRender(t, d), mustRender(t, intraB2C(t))) {
		t.Error("different documents rendered identically")
	}
}

func TestRupeeGlyphIsEmbeddedNotSubstituted(t *testing.T) {
	// If the font lacked U+20B9 the extractor would not find it.
	if !strings.Contains(text(t, mustRender(t, intraB2C(t))), "₹") {
		t.Fatal("no rupee sign in output")
	}
}

func TestLogoIsOptional(t *testing.T) {
	d := intraB2C(t)
	d.Supplier.Logo = nil
	mustRender(t, d)
	d.Supplier.Logo = []byte("not a png")
	if _, err := Render(d, Options{}); err == nil {
		t.Fatal("garbage logo accepted")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]func(*Document){
		"no number":       func(d *Document) { d.Number = "" },
		"bad kind":        func(d *Document) { d.Kind = "receipt" },
		"no lines":        func(d *Document) { d.Lines = nil },
		"totals mismatch": func(d *Document) { d.Totals.Total++ },
		"unknown version": func(d *Document) { d.TemplateVersion = "v999" },
		"credit w/o original": func(d *Document) {
			d.Kind = KindCreditNote
			d.OriginalNumber = ""
		},
		"export w/o LUT number": func(d *Document) {
			*d = exportLUT(t)
			d.LUTNumber = ""
		},
	}
	for name, mut := range cases {
		d := intraB2C(t)
		mut(&d)
		if _, err := Render(d, Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type fakeSigner struct{ called bool }

func (f *fakeSigner) Sign(pdfBytes []byte) ([]byte, error) {
	f.called = true
	return append(append([]byte(nil), pdfBytes...), []byte("\n%signed")...), nil
}

func TestSignerHook(t *testing.T) {
	s := &fakeSigner{}
	b, err := Render(intraB2C(t), Options{Signer: s})
	if err != nil || !s.called || !bytes.HasSuffix(b, []byte("%signed")) {
		t.Fatalf("signer not applied: %v", err)
	}
	failing := signerFunc(func([]byte) ([]byte, error) { return nil, errors.New("hsm down") })
	if _, err := Render(intraB2C(t), Options{Signer: failing}); err == nil {
		t.Fatal("signer error swallowed")
	}
}

type signerFunc func([]byte) ([]byte, error)

func (f signerFunc) Sign(b []byte) ([]byte, error) { return f(b) }

func TestFormatINR(t *testing.T) {
	for in, want := range map[int64]string{0: "₹0.00", 5: "₹0.05", 100: "₹1.00", 123456789: "₹12,34,567.89",
		99999: "₹999.99", 10000000: "₹1,00,000.00", -59000: "-₹590.00"} {
		if got := formatINR(in); got != want {
			t.Errorf("formatINR(%d)=%q want %q", in, got, want)
		}
	}
}

func TestFixtureNumbersMatchDocumentNumber(t *testing.T) {
	for want, got := range map[string]string{
		"VS/2627/000042":   intraB2C(t).Number,
		"VS/2627/000043":   interB2B(t).Number,
		"VSCN/2627/000007": creditNote(t).Number,
	} {
		prefix, rest, _ := strings.Cut(want, "/")
		var seq int64
		for _, c := range rest[5:] {
			seq = seq*10 + int64(c-'0')
		}
		n, err := tax.DocumentNumber(prefix, "2627", seq)
		if err != nil || n != want || got != want {
			t.Errorf("fixture %q vs DocumentNumber %q (%v)", got, n, err)
		}
	}
}

func TestNumberLongerThan16CharsRejected(t *testing.T) {
	d := intraB2C(t)
	d.Number = "VS/2026-27/000042" // 17 characters: breaks Rule 46
	if _, err := Render(d, Options{}); err == nil || !strings.Contains(err.Error(), "16") {
		t.Fatalf("err=%v", err)
	}
}

func TestLogoDoesNotOverlapSupplierText(t *testing.T) {
	d := intraB2C(t)
	_, l, err := render(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Logo.W == 0 {
		t.Fatal("logo not placed")
	}
	if l.Logo.H > 14.01 {
		t.Errorf("logo %.1fmm tall, max 14", l.Logo.H)
	}
	if l.SupplierTextX <= l.Logo.X+l.Logo.W {
		t.Errorf("supplier text x=%.1f starts inside logo (right edge %.1f)", l.SupplierTextX, l.Logo.X+l.Logo.W)
	}
	// the logo stays inside the supplier box, below the title
	if l.Logo.Y < l.TitleBottom || l.Logo.Y+l.Logo.H > l.SupplierBoxBottom {
		t.Errorf("logo %+v outside supplier box (title bottom %.1f, box bottom %.1f)", l.Logo, l.TitleBottom, l.SupplierBoxBottom)
	}
	// a tall logo keeps its aspect ratio within the 14mm cap
	tall := image.NewRGBA(image.Rect(0, 0, 10, 40))
	var b bytes.Buffer
	_ = png.Encode(&b, tall)
	d.Supplier.Logo = b.Bytes()
	_, l, err = render(d, Options{})
	if err != nil || l.Logo.H > 14.01 || l.Logo.W < 3 || l.Logo.W > 3.6 {
		t.Fatalf("tall logo %+v err=%v", l.Logo, err)
	}
	// without a logo the text starts at the box edge
	d.Supplier.Logo = nil
	_, l, _ = render(d, Options{})
	if l.Logo.W != 0 || l.SupplierTextX >= 14 {
		t.Errorf("no-logo layout %+v", l)
	}
}
