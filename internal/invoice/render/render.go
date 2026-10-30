package render

import (
	"bytes"
	_ "embed"
	"fmt"
	"strconv"
	"strings"

	"github.com/go-pdf/fpdf"

	"github.com/velixio/paymux/internal/qrcode"
	"github.com/velixio/paymux/internal/tax"
)

// Noto Sans (SIL OFL 1.1, see fonts/OFL.txt) carries the rupee sign U+20B9.
var (
	//go:embed fonts/NotoSans-Regular.ttf
	fontRegular []byte
	//go:embed fonts/NotoSans-Bold.ttf
	fontBold []byte
)

const (
	marginX   = 12.0
	pageW     = 210.0
	contentW  = pageW - 2*marginX
	lineH     = 4.4
	pageLimit = 275.0
)

// Render produces the PDF bytes. The output depends only on d (the creation
// date is the issue date), so identical input renders identical bytes.
func Render(d Document, o Options) ([]byte, error) {
	b, _, err := render(d, o)
	return b, err
}

// rect is a placed box in millimetres.
type rect struct{ X, Y, W, H float64 }

// layout records where things landed, for tests.
type layout struct {
	Logo              rect
	SupplierTextX     float64
	TitleBottom       float64
	SupplierBoxBottom float64
}

const (
	logoMaxH = 14.0
	logoMaxW = 30.0
)

func render(d Document, o Options) ([]byte, layout, error) {
	if err := d.validate(); err != nil {
		return nil, layout{}, fmt.Errorf("invoice render: %w", err)
	}
	p := fpdf.New("P", "mm", "A4", "")
	p.SetCompression(true)
	p.SetCatalogSort(true)
	p.SetCreationDate(d.IssueDate.UTC())
	p.SetModificationDate(d.IssueDate.UTC())
	p.SetMargins(marginX, 12, marginX)
	p.SetAutoPageBreak(false, 0)
	p.SetTitle(title(d)+" "+d.Number, true)
	p.SetAuthor(firstNonEmpty(d.Supplier.LegalName, d.Supplier.TradeName), true)
	p.SetCreator("Paymux invoice template "+d.TemplateVersion, true)
	p.AddUTF8FontFromBytes("noto", "", fontRegular)
	p.AddUTF8FontFromBytes("noto", "B", fontBold)
	p.AddPage()

	r := &renderer{p: p, d: d}
	if err := r.draw(); err != nil {
		return nil, layout{}, fmt.Errorf("invoice render: %w", err)
	}
	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		return nil, layout{}, fmt.Errorf("invoice render: %w", err)
	}
	out := buf.Bytes()
	if o.Signer != nil {
		signed, err := o.Signer.Sign(out)
		if err != nil {
			return nil, layout{}, fmt.Errorf("invoice render: sign: %w", err)
		}
		out = signed
	}
	return out, r.lay, nil
}

type renderer struct {
	p *fpdf.Fpdf
	d Document
	y float64

	lay layout
	err error
}

func title(d Document) string {
	if d.Kind == KindCreditNote {
		return "CREDIT NOTE"
	}
	return "TAX INVOICE"
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func (r *renderer) font(style string, size float64) { r.p.SetFont("noto", style, size) }

// para writes wrapped text in a column and returns its height.
func (r *renderer) para(x, y, w float64, style string, size float64, s string) float64 {
	r.font(style, size)
	lines := r.p.SplitText(s, w)
	for i, l := range lines {
		r.p.SetXY(x, y+float64(i)*lineH)
		r.p.CellFormat(w, lineH, l, "", 0, "L", false, 0, "")
	}
	return float64(len(lines)) * lineH
}

func (r *renderer) draw() error {
	d, p := r.d, r.p
	r.font("B", 15)
	p.SetXY(marginX, 12)
	p.CellFormat(contentW, 8, title(d), "", 0, "C", false, 0, "")
	r.lay.TitleBottom = 20
	r.y = 24

	// Supplier (left) and document details (right).
	half := contentW/2 - 2
	left := r.supplierBlock(marginX, r.y, half)
	right := r.metaBlock(marginX+half+4, r.y, half)
	if r.err != nil {
		return r.err
	}
	h := max(left, right)
	r.lay.SupplierBoxBottom = r.y - 1 + h + 2
	p.Rect(marginX, r.y-1, half, h+2, "D")
	p.Rect(marginX+half+4, r.y-1, half, h+2, "D")
	r.y += h + 4

	rh := r.recipientBlock(marginX, r.y, contentW)
	p.Rect(marginX, r.y-1, contentW, rh+2, "D")
	r.y += rh + 4

	r.table()
	r.totals()
	r.notes()
	r.signature()
	return p.Error()
}

func (r *renderer) supplierBlock(x, y, w float64) float64 {
	s := r.d.Supplier
	tx, tw, logoH := x+1, w-2, 0.0
	if len(s.Logo) > 0 {
		lw, lh, err := r.placeLogo(x+1, y, s.Logo)
		if err != nil {
			r.err = err
			return 0
		}
		tx += lw + 3
		tw -= lw + 3
		logoH = lh
	}
	r.lay.SupplierTextX = tx
	h := r.para(tx, y, tw, "B", 10, firstNonEmpty(s.TradeName, s.LegalName))
	if s.TradeName != "" && s.LegalName != "" && s.LegalName != s.TradeName {
		h += r.para(tx, y+h, tw, "", 8.5, s.LegalName)
	}
	h += r.para(tx, y+h, tw, "", 8.5, s.Address)
	h += r.para(tx, y+h, tw, "", 8.5, "GSTIN: "+s.GSTIN)
	h += r.para(tx, y+h, tw, "", 8.5, fmt.Sprintf("State: %s, Code: %s", s.StateName, s.StateCode))
	return max(h, logoH)
}

// placeLogo draws the logo at (x, y) scaled to fit logoMaxW x logoMaxH while
// keeping its aspect ratio, and returns the size used.
func (r *renderer) placeLogo(x, y float64, png []byte) (w, h float64, err error) {
	opts := fpdf.ImageOptions{ImageType: "PNG"}
	info := r.p.RegisterImageOptionsReader("logo", opts, bytes.NewReader(png))
	if e := r.p.Error(); e != nil {
		return 0, 0, fmt.Errorf("logo: %w", e)
	}
	if info == nil {
		return 0, 0, fmt.Errorf("logo: unreadable image")
	}
	iw, ih := info.Extent()
	if iw <= 0 || ih <= 0 {
		return 0, 0, fmt.Errorf("logo: empty image")
	}
	scale := min(logoMaxW/iw, logoMaxH/ih)
	w, h = iw*scale, ih*scale
	r.p.ImageOptions("logo", x, y, w, h, false, opts, 0, "")
	r.lay.Logo = rect{X: x, Y: y, W: w, H: h}
	return w, h, nil
}

func (r *renderer) metaBlock(x, y, w float64) float64 {
	d := r.d
	h := 0.0
	row := func(k, v string) {
		if v == "" {
			return
		}
		h += r.para(x+1, y+h, w-2, "", 8.5, k+": "+v)
	}
	if d.Kind == KindCreditNote {
		row("Credit Note No", d.Number)
		row("Credit Note Date", fmtDate(d))
		row("Original Invoice No", d.OriginalNumber)
		if !d.OriginalDate.IsZero() {
			row("Original Invoice Date", d.OriginalDate.In(tax.IST).Format("02-01-2006"))
		}
		row("Reason", d.Reason)
	} else {
		row("Invoice No", d.Number)
		row("Invoice Date", fmtDate(d))
	}
	row("Place of Supply", d.PlaceOfSupply)
	row("Whether tax is payable on reverse charge", "No")
	return h
}

func fmtDate(d Document) string { return d.IssueDate.In(tax.IST).Format("02-01-2006") }

func (r *renderer) recipientBlock(x, y, w float64) float64 {
	c := r.d.Recipient
	h := r.para(x+1, y, w-2, "B", 8.5, "Bill to")
	h += r.para(x+1, y+h, w-2, "B", 10, c.Name)
	h += r.para(x+1, y+h, w-2, "", 8.5, c.Address)
	if c.GSTIN != "" {
		h += r.para(x+1, y+h, w-2, "", 8.5, "GSTIN: "+c.GSTIN)
	}
	switch {
	case c.StateName != "":
		h += r.para(x+1, y+h, w-2, "", 8.5, fmt.Sprintf("State: %s, Code: %s", c.StateName, c.StateCode))
	case c.CountryName != "" || c.Country != "":
		h += r.para(x+1, y+h, w-2, "", 8.5, "Country: "+firstNonEmpty(c.CountryName, c.Country))
	}
	return h
}

type col struct {
	head  string
	w     float64
	align string
}

func (r *renderer) columns() []col {
	cols := []col{{"Description", 0, "L"}, {"SAC", 15, "C"}, {"Qty / Unit", 17, "C"}, {"Taxable value", 25, "R"}}
	if r.d.InterState {
		cols = append(cols, col{"IGST rate", 14, "R"}, col{"IGST amount", 24, "R"})
	} else {
		cols = append(cols, col{"CGST rate", 13, "R"}, col{"CGST amount", 20, "R"}, col{"SGST rate", 13, "R"}, col{"SGST amount", 20, "R"})
	}
	fixed := 0.0
	for _, c := range cols[1:] {
		fixed += c.w
	}
	cols[0].w = contentW - fixed
	return cols
}

func (r *renderer) tableHeader(cols []col) {
	r.font("B", 7.5)
	r.p.SetFillColor(235, 235, 235)
	x := marginX
	for _, c := range cols {
		r.p.SetXY(x, r.y)
		r.p.CellFormat(c.w, 6, c.head, "1", 0, "C", true, 0, "")
		x += c.w
	}
	r.y += 6
}

func (r *renderer) table() {
	cols := r.columns()
	r.tableHeader(cols)
	for _, l := range r.d.Lines {
		cells := []string{l.Description, l.SAC, fmt.Sprintf("%d %s", l.Qty, l.Unit), formatINR(l.Taxable)}
		if r.d.InterState {
			cells = append(cells, formatRate(l.IGSTRateBP), formatINR(l.IGST))
		} else {
			cells = append(cells, formatRate(l.CGSTRateBP), formatINR(l.CGST), formatRate(l.SGSTRateBP), formatINR(l.SGST))
		}
		r.font("", 8)
		lines := r.p.SplitText(cells[0], cols[0].w-2)
		h := max(float64(len(lines)), 1) * lineH
		if r.y+h+2 > pageLimit {
			r.p.AddPage()
			r.y = 12
			r.tableHeader(cols)
		}
		x := marginX
		for i, c := range cols {
			r.p.Rect(x, r.y, c.w, h+2, "D")
			if i == 0 {
				for j, ln := range lines {
					r.p.SetXY(x+1, r.y+1+float64(j)*lineH)
					r.p.CellFormat(c.w-2, lineH, ln, "", 0, "L", false, 0, "")
				}
			} else {
				r.p.SetXY(x+1, r.y+1)
				r.p.CellFormat(c.w-2, lineH, cells[i], "", 0, c.align, false, 0, "")
			}
			x += c.w
		}
		r.y += h + 2
	}
	r.y += 3
}

func (r *renderer) totals() {
	d := r.d
	if r.y+45 > pageLimit {
		r.p.AddPage()
		r.y = 12
	}
	taxLabel, valLabel := "Total tax", "Total taxable value"
	if d.Kind == KindCreditNote {
		valLabel, taxLabel = "Taxable value credited", "Tax credited"
	}
	totalLabel := "Total invoice value"
	if d.Kind == KindCreditNote {
		totalLabel = "Total credit note value"
	}
	rows := [][2]string{{valLabel, formatINR(d.Totals.Taxable)}}
	if d.InterState {
		rows = append(rows, [2]string{"IGST", formatINR(d.Totals.IGST)})
	} else {
		rows = append(rows, [2]string{"CGST", formatINR(d.Totals.CGST)}, [2]string{"SGST", formatINR(d.Totals.SGST)})
	}
	rows = append(rows, [2]string{taxLabel, formatINR(d.Totals.Tax())})
	x, w := marginX+contentW-80, 80.0
	for _, row := range rows {
		r.font("", 8.5)
		r.p.SetXY(x, r.y)
		r.p.CellFormat(w/2, lineH+0.6, row[0], "B", 0, "L", false, 0, "")
		r.p.CellFormat(w/2, lineH+0.6, row[1], "B", 0, "R", false, 0, "")
		r.y += lineH + 0.6
	}
	r.font("B", 10)
	r.p.SetXY(x, r.y)
	r.p.CellFormat(w/2, 6.5, totalLabel, "B", 0, "L", false, 0, "")
	r.p.CellFormat(w/2, 6.5, formatINR(d.Totals.Total), "B", 0, "R", false, 0, "")
	r.y += 8
	r.y += r.para(marginX, r.y, contentW, "B", 8.5, "Total in words: "+tax.AmountInWordsINR(d.Totals.Total)) + 2
}

func (r *renderer) notes() {
	d := r.d
	if d.Currency != "" {
		r.y += r.para(marginX, r.y, contentW, "", 8.5, fmt.Sprintf("Amount received: %s %s at exchange rate 1 %s = INR %s",
			d.Currency, formatMinor(d.ForeignAmount), d.Currency, d.FXRate)) + 1
	}
	if d.ExportEndorsement != "" || d.LUTNumber != "" {
		if d.ExportEndorsement != "" {
			r.y += r.para(marginX, r.y, contentW, "B", 8.5, d.ExportEndorsement) + 1
		}
		if d.LUTNumber != "" {
			r.y += r.para(marginX, r.y, contentW, "", 8.5, "LUT No: "+d.LUTNumber) + 1
		}
	}
	if d.PaymentMethod != "" || d.PaymentRef != "" {
		s := "Payment method: " + d.PaymentMethod
		if d.PaymentRef != "" {
			s += "   Reference: " + d.PaymentRef
		}
		r.y += r.para(marginX, r.y, contentW, "", 8.5, s) + 1
	}
	r.eInvoice()
	r.y += 4
}

// qrModule is the side of one QR module in millimetres. The portal's signed
// code is about 120 modules wide, so this keeps it near 42 mm and scannable.
const qrModule = 0.34

// eInvoice prints the IRN and acknowledgement of a registered e-invoice with
// its QR code, drawn as filled rectangles (vector, no image).
func (r *renderer) eInvoice() {
	e := r.d.EInvoice
	if e == nil || r.d.Kind != KindInvoice {
		return
	}
	code, err := qrcode.New(e.QR)
	if err != nil {
		code = nil // too long for a QR code: the IRN text still identifies the invoice
	}
	qrMM := 0.0
	if code != nil {
		qrMM = float64(code.ViewBox()) * qrModule
	}
	h := max(qrMM, 24)
	if r.y+h+8 > pageLimit {
		r.p.AddPage()
		r.y = 12
	}
	r.y += 2
	r.font("B", 9)
	r.p.SetXY(marginX, r.y)
	r.p.CellFormat(contentW, 5, "e-Invoice", "", 0, "L", false, 0, "")
	y0 := r.y + 6
	textX := float64(marginX)
	if code != nil {
		r.p.SetFillColor(0, 0, 0)
		const qz = 4
		for y := 0; y < code.Size; y++ {
			for x := 0; x < code.Size; {
				if !code.Black(x, y) {
					x++
					continue
				}
				start := x
				for x < code.Size && code.Black(x, y) {
					x++
				}
				r.p.Rect(marginX+float64(start+qz)*qrModule, y0+float64(y+qz)*qrModule, float64(x-start)*qrModule, qrModule, "F")
			}
		}
		r.p.SetFillColor(255, 255, 255)
		textX = marginX + qrMM + 4
	}
	w := marginX + contentW - textX
	th := r.para(textX, y0, w, "B", 8, "IRN (Invoice Reference Number)")
	th += r.para(textX, y0+th, w, "", 8, e.IRN)
	th += r.para(textX, y0+th, w, "", 8, "Ack No: "+e.AckNo)
	th += r.para(textX, y0+th, w, "", 8, "Ack Date: "+e.AckDate.In(tax.IST).Format("02-01-2006 15:04:05"))
	r.y = y0 + max(qrMM, th) + 2
}

func (r *renderer) signature() {
	d := r.d
	if r.y+40 > pageLimit {
		r.p.AddPage()
		r.y = 12
	}
	x, w := marginX+contentW-80, 80.0
	y := r.y
	h := r.para(x, y, w, "B", 8.5, "For "+firstNonEmpty(d.Supplier.LegalName, d.Supplier.TradeName))
	h += 10
	if d.SignatoryName != "" {
		h += r.para(x, y+h, w, "", 8.5, "Authorised signatory: "+d.SignatoryName)
	}
	if d.SignedBy != "" {
		h += r.para(x, y+h, w, "", 7.5, "Digitally signed by "+d.SignedBy)
	} else {
		h += r.para(x, y+h, w, "", 7.5, "This is a computer-generated document")
	}
	r.y = y + h + 4
	if d.FooterNote != "" {
		r.y += r.para(marginX, r.y, contentW, "", 8, d.FooterNote) + 2
	}
	r.font("", 6.5)
	r.p.SetXY(marginX, 287)
	r.p.CellFormat(contentW, 3, "Template "+d.TemplateVersion, "", 0, "R", false, 0, "")
}

// formatRate prints basis points as a percentage: 900 -> "9%", 250 -> "2.5%".
func formatRate(bp int) string {
	if bp%100 == 0 {
		return strconv.Itoa(bp/100) + "%"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(bp)/100), "0"), ".") + "%"
}

// formatMinor prints minor units as "12.00" (no grouping).
func formatMinor(v int64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// formatINR prints paise as rupees with Indian digit grouping: "₹12,34,567.89".
func formatINR(paise int64) string {
	sign := ""
	if paise < 0 {
		sign, paise = "-", -paise
	}
	whole := strconv.FormatInt(paise/100, 10)
	if len(whole) > 3 {
		head, tail := whole[:len(whole)-3], whole[len(whole)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		whole = strings.Join(parts, ",") + "," + tail
	}
	return fmt.Sprintf("%s₹%s.%02d", sign, whole, paise%100)
}
