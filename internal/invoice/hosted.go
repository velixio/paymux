package invoice

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/velixio/paymux/internal/domains"
	"net/http"
	"net/url"
	"strings"

	"github.com/velixio/paymux/internal/look"
	"github.com/velixio/paymux/internal/qrcode"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/tax"
	"github.com/velixio/paymux/pkg/connector/money"
)

type pageLine struct{ Description, SAC, Taxable string }
type pageTax struct{ Label, Amount string }

// docPage is the view model of web/templates/invoice.html (plus the layout's
// AppName, LogoURL and Accent).
type docPage struct {
	AppName, LogoURL, Accent                        string
	Title, Number, Date                             string
	OriginalNumber, OriginalDate, Reason            string
	SupplierName, SupplierAddress, SupplierGSTIN    string
	RecipientName, RecipientAddress, RecipientGSTIN string
	PlaceOfSupply                                   string
	Lines                                           []pageLine
	Taxes                                           []pageTax
	Total, TotalWords                               string
	Notes                                           []string
	PDFURL                                          string
	// EInvoice is set once the operator has recorded the portal's answer.
	EInvoice *eInvoicePage
}

// eInvoicePage is the e-invoice block of the hosted page: the QR code is an
// inline SVG (no image request, nothing external).
type eInvoicePage struct {
	IRN, AckNo, AckDate string
	QRPath              string // SVG path data; empty when the token is too long for a QR code
	QRBox               int
}

func fmtMoney(minor int64, cur string) string {
	s, err := money.FormatDecimal(minor, cur)
	if err != nil {
		return fmt.Sprintf("%d", minor)
	}
	if cur == "INR" {
		return "INR " + s
	}
	return cur + " " + s
}

// ServeHosted serves GET /i/{id} (HTML) and GET /i/{id}.pdf (PDF). Access is by
// the unguessable ?t= token, compared by hash in constant time; any failure
// answers 404 so ids cannot be probed.
func (s *Service) ServeHosted(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	id := r.PathValue("id")
	wantPDF := strings.HasSuffix(id, ".pdf")
	id = strings.TrimSuffix(id, ".pdf")
	token := r.URL.Query().Get("t")
	if token == "" {
		http.NotFound(w, r)
		return
	}
	got := hashToken(token)

	var (
		number, hash, blobID string
		appID                string
		page                 docPage
	)
	err := s.db.Read(r.Context(), func(q store.Querier) error {
		switch {
		case strings.HasPrefix(id, "inv_"):
			inv, err := store.GetInvoice(r.Context(), q, id)
			if err != nil {
				return err
			}
			number, hash, blobID, appID = inv.Number, inv.AccessTokenHash, inv.PDFBlobID, inv.AppID
			if subtle.ConstantTimeCompare([]byte(got), []byte(hash)) != 1 || wantPDF {
				return nil
			}
			page, err = s.invoicePage(r.Context(), q, inv)
			return err
		case strings.HasPrefix(id, "crn_"):
			cn, err := store.GetCreditNote(r.Context(), q, id)
			if err != nil {
				return err
			}
			number, hash, blobID, appID = cn.Number, cn.AccessTokenHash, cn.PDFBlobID, cn.AppID
			if subtle.ConstantTimeCompare([]byte(got), []byte(hash)) != 1 || wantPDF {
				return nil
			}
			page, err = s.creditNotePage(r.Context(), q, cn)
			return err
		}
		return store.ErrNotFound
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case subtle.ConstantTimeCompare([]byte(got), []byte(hash)) != 1:
		http.NotFound(w, r)
		return
	}
	// A custom domain serves only its own app's documents.
	if hostApp, onCustom := domains.AppFrom(r.Context()); onCustom && hostApp != appID {
		http.NotFound(w, r)
		return
	}

	if wantPDF {
		pdf, err := s.PDF(r.Context(), blobID)
		switch {
		case errors.Is(err, ErrPDFNotReady):
			w.Header().Set("Retry-After", "5")
			http.Error(w, "the PDF is being prepared", http.StatusNotFound)
		case err != nil:
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", strings.ReplaceAll(number, "/", "-")+".pdf"))
			_, _ = w.Write(pdf)
		}
		return
	}
	if blobID != "" {
		page.PDFURL = "/i/" + id + ".pdf?t=" + url.QueryEscape(token)
	}
	pages, err := s.pages()
	if err == nil {
		var buf bytes.Buffer
		// The page wears the app's theme. It is a legal document, so its words are not translated.
		themed := look.Resolve(r.Context(), s.settings, pages, appID, "").Bind(pages)
		if err = themed.Render(&buf, "invoice", page); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
			_, _ = w.Write(buf.Bytes())
			return
		}
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Service) invoicePage(ctx context.Context, q store.Queryer, inv store.Invoice) (docPage, error) {
	var sup Supplier
	var rcp Recipient
	if err := json.Unmarshal(inv.Supplier, &sup); err != nil {
		return docPage{}, err
	}
	if err := json.Unmarshal(inv.Recipient, &rcp); err != nil {
		return docPage{}, err
	}
	p := docPage{Title: "Tax invoice", Number: inv.Number, Date: inv.IssuedAt.In(tax.IST).Format("02 Jan 2006"),
		SupplierName: firstNonEmpty(sup.LegalName, sup.TradeName), SupplierAddress: sup.Address, SupplierGSTIN: sup.GSTIN,
		RecipientName: rcp.Name, RecipientAddress: rcp.Address, RecipientGSTIN: rcp.GSTIN, PlaceOfSupply: placeOfSupply(inv.PlaceOfSupply, rcp)}
	p.fill(inv.Lines, inv.Currency, inv.Taxable, inv.CGST, inv.SGST, inv.IGST, inv.Total)
	if inv.SupplyType == string(tax.ExportLUT) {
		p.Notes = append(p.Notes, exportEndorsement, "LUT No: "+inv.LUTNumber)
	}
	if inv.IRN != "" {
		e := &eInvoicePage{IRN: inv.IRN, AckNo: inv.AckNo, AckDate: inv.AckDate.In(tax.IST).Format("02-01-2006 15:04:05")}
		if code, err := qrcode.New(inv.SignedQR); err == nil {
			e.QRPath, e.QRBox = code.Path(), code.ViewBox()
		}
		p.EInvoice = e
	}
	s.branding(ctx, q, inv.AppID, &p)
	return p, nil
}

func (s *Service) creditNotePage(ctx context.Context, q store.Queryer, cn store.CreditNote) (docPage, error) {
	inv, err := store.GetInvoice(ctx, q, cn.InvoiceID)
	if err != nil {
		return docPage{}, err
	}
	p, err := s.invoicePage(ctx, q, inv)
	if err != nil {
		return docPage{}, err
	}
	p.EInvoice = nil // the IRN belongs to the invoice
	p.Title, p.Number, p.Date = "Credit note", cn.Number, cn.IssuedAt.In(tax.IST).Format("02 Jan 2006")
	p.OriginalNumber, p.OriginalDate, p.Reason = inv.Number, inv.IssuedAt.In(tax.IST).Format("02 Jan 2006"), cn.Reason
	p.Notes = nil
	p.fill(cn.Lines, cn.Currency, cn.Taxable, cn.CGST, cn.SGST, cn.IGST, cn.Total)
	return p, nil
}

func placeOfSupply(code string, rcp Recipient) string {
	if code == tax.PlaceOutsideIndia {
		return "Outside India (" + firstNonEmpty(rcp.CountryName, rcp.Country) + ")"
	}
	name, _ := tax.StateName(code)
	return fmt.Sprintf("%s (%s)", name, code)
}

func (p *docPage) fill(rawLines json.RawMessage, cur string, taxable, cgst, sgst, igst, total int64) {
	var lines []Line
	_ = json.Unmarshal(rawLines, &lines)
	p.Lines = nil
	for _, l := range lines {
		p.Lines = append(p.Lines, pageLine{l.Description, l.SAC, fmtMoney(l.Taxable, cur)})
	}
	p.Taxes = []pageTax{{"Taxable value", fmtMoney(taxable, cur)}}
	if cgst != 0 || sgst != 0 {
		p.Taxes = append(p.Taxes, pageTax{"CGST", fmtMoney(cgst, cur)}, pageTax{"SGST", fmtMoney(sgst, cur)})
	}
	if igst != 0 {
		p.Taxes = append(p.Taxes, pageTax{"IGST", fmtMoney(igst, cur)})
	}
	p.Total = fmtMoney(total, cur)
	p.TotalWords = ""
	if cur == "INR" {
		p.TotalWords = tax.AmountInWordsINR(total)
	}
}

// branding fills the layout fields from the app (name and accent colour).
func (s *Service) branding(ctx context.Context, q store.Queryer, appID string, p *docPage) {
	if app, err := store.GetApp(ctx, q, appID); err == nil {
		p.AppName = app.Name
	}
}
