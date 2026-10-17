// Package einvoice builds the Indian GST e-invoice document (IRP schema
// INV-01, version 1.1) from an issued invoice, for upload to the Invoice
// Registration Portal by hand ("offline mode"): the operator downloads the
// JSON, uploads it to the portal and pastes the IRN, acknowledgement and
// signed QR code back.
//
// The package knows nothing about the database. The caller fills a Source
// with INR paise (the same figures the PDF prints); Build maps it onto the
// portal's structure and Validate checks the result against the committed
// JSON Schema (schema/einvoice-1.1.json) and the arithmetic the portal checks.
package einvoice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/paymux/paymux/internal/tax"
	"github.com/paymux/paymux/pkg/connector/money"
)

// Version is the schema version produced.
const Version = "1.1"

// Supply types of a Source (the values of tax.SupplyType).
const (
	B2B        = "b2b"
	ExportLUT  = "export_lut"
	ExportIGST = "export_igst"
)

// ErrNotEligible means the invoice is not one that gets an e-invoice (B2C).
var ErrNotEligible = errors.New("einvoice: only B2B and export invoices have an e-invoice")

// Eligible reports whether invoices of this supply type get an e-invoice.
func Eligible(supplyType string) bool {
	return supplyType == B2B || supplyType == ExportLUT || supplyType == ExportIGST
}

// Problems lists everything that stops a document from being built, in words
// an operator can act on.
type Problems struct{ List []string }

func (p *Problems) Error() string { return "e-invoice: " + strings.Join(p.List, "; ") }

// Party is a seller or a buyer as the invoice stores it. Structured address
// fields win over Address, which is free text split on commas.
type Party struct {
	GSTIN, LegalName, TradeName string
	Address                     string
	Line1, Line2, City          string
	PostalCode                  string
	StateCode                   string // 2-digit
	Country, CountryName        string // buyers outside India: ISO alpha-2
	Phone, Email                string
}

// Line is one invoice line in INR paise.
type Line struct {
	Description, SAC                   string
	Qty                                int64
	Taxable                            int64
	CGSTRateBP, SGSTRateBP, IGSTRateBP int
	CGST, SGST, IGST                   int64
}

// CreditRef says that a Source is a credit note, and which invoice it answers.
type CreditRef struct {
	InvoiceNumber string
	InvoiceDate   time.Time
}

// Source is everything Build needs. With Credit set it describes a credit
// note: Number and IssuedAt are the credit note's, the lines its own (positive)
// amounts, and the portal document is a CRN referring to the invoice.
type Source struct {
	Credit     *CreditRef
	Number     string
	IssuedAt   time.Time
	SupplyType string // B2B, ExportLUT or ExportIGST
	// PlaceOfSupply is the buyer's 2-digit state code (ignored for exports).
	PlaceOfSupply string
	Seller, Buyer Party
	Lines         []Line
	// Exports: the invoice currency and its total in that currency's minor units.
	Currency     string
	ForeignTotal int64
}

// ---- the document ----

// Invoice is the INV-01 document. Field order follows the portal's.
type Invoice struct {
	Version    string     `json:"Version"`
	TranDtls   TranDtls   `json:"TranDtls"`
	DocDtls    DocDtls    `json:"DocDtls"`
	SellerDtls SellerDtls `json:"SellerDtls"`
	BuyerDtls  BuyerDtls  `json:"BuyerDtls"`
	ItemList   []Item     `json:"ItemList"`
	ValDtls    ValDtls    `json:"ValDtls"`
	ExpDtls    *ExpDtls   `json:"ExpDtls,omitempty"`
	RefDtls    *RefDtls   `json:"RefDtls,omitempty"`
}

// RefDtls are the references of a credit note to the documents it answers.
type RefDtls struct {
	PrecDocDtls []PrecDoc `json:"PrecDocDtls"`
}

// PrecDoc is the preceding document: the invoice a credit note is against.
type PrecDoc struct {
	InvNo string `json:"InvNo"`
	InvDt string `json:"InvDt"`
}

// TranDtls are the transaction details.
type TranDtls struct {
	TaxSch      string `json:"TaxSch"`
	SupTyp      string `json:"SupTyp"`
	RegRev      string `json:"RegRev"`
	IgstOnIntra string `json:"IgstOnIntra"`
}

// DocDtls identify the document.
type DocDtls struct {
	Typ string `json:"Typ"`
	No  string `json:"No"`
	Dt  string `json:"Dt"`
}

// SellerDtls is the supplier.
type SellerDtls struct {
	Gstin string `json:"Gstin"`
	LglNm string `json:"LglNm"`
	TrdNm string `json:"TrdNm,omitempty"`
	Addr1 string `json:"Addr1"`
	Addr2 string `json:"Addr2,omitempty"`
	Loc   string `json:"Loc"`
	Pin   int    `json:"Pin"`
	Stcd  string `json:"Stcd"`
	Ph    string `json:"Ph,omitempty"`
	Em    string `json:"Em,omitempty"`
}

// BuyerDtls is the recipient.
type BuyerDtls struct {
	Gstin string `json:"Gstin"`
	LglNm string `json:"LglNm"`
	Pos   string `json:"Pos"`
	Addr1 string `json:"Addr1"`
	Addr2 string `json:"Addr2,omitempty"`
	Loc   string `json:"Loc"`
	Pin   int    `json:"Pin"`
	Stcd  string `json:"Stcd"`
}

// Item is one line.
type Item struct {
	SlNo       string      `json:"SlNo"`
	PrdDesc    string      `json:"PrdDesc"`
	IsServc    string      `json:"IsServc"`
	HsnCd      string      `json:"HsnCd"`
	Qty        json.Number `json:"Qty"`
	Unit       string      `json:"Unit"`
	UnitPrice  json.Number `json:"UnitPrice"`
	TotAmt     json.Number `json:"TotAmt"`
	AssAmt     json.Number `json:"AssAmt"`
	GstRt      json.Number `json:"GstRt"`
	IgstAmt    json.Number `json:"IgstAmt"`
	CgstAmt    json.Number `json:"CgstAmt"`
	SgstAmt    json.Number `json:"SgstAmt"`
	TotItemVal json.Number `json:"TotItemVal"`
}

// ValDtls are the document totals.
type ValDtls struct {
	AssVal      json.Number `json:"AssVal"`
	CgstVal     json.Number `json:"CgstVal"`
	SgstVal     json.Number `json:"SgstVal"`
	IgstVal     json.Number `json:"IgstVal"`
	TotInvVal   json.Number `json:"TotInvVal"`
	TotInvValFc json.Number `json:"TotInvValFc,omitempty"`
}

// ExpDtls are the export details.
type ExpDtls struct {
	RefClm  string `json:"RefClm"`
	ForCur  string `json:"ForCur"`
	CntCode string `json:"CntCode"`
}

var (
	numberRE   = regexp.MustCompile(`^[A-Z1-9][A-Z0-9/-]{0,15}$`)
	gstinRE    = regexp.MustCompile(`^[0-9]{2}[0-9A-Z]{13}$`)
	sacRE      = regexp.MustCompile(`^[0-9]{4,8}$`)
	stateRE    = regexp.MustCompile(`^[0-9]{2}$`)
	countryRE  = regexp.MustCompile(`^[A-Za-z]{2}$`)
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
)

// paise renders paise as rupees with two decimals.
func paise(v int64) json.Number {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return json.Number(fmt.Sprintf("%s%d.%02d", sign, v/100, v%100))
}

// percent renders basis points as a percentage without trailing zeros.
func percent(bp int) json.Number {
	whole, frac := bp/100, bp%100
	if frac == 0 {
		return json.Number(strconv.Itoa(whole))
	}
	s := fmt.Sprintf("%d.%02d", whole, frac)
	return json.Number(strings.TrimRight(s, "0"))
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func phone(s string) string {
	d := digits(s)
	if len(d) < 6 || len(d) > 12 {
		return ""
	}
	return d
}

// Build maps the source onto the INV-01 document. Every problem found is
// reported at once as *Problems.
func Build(src Source) (*Invoice, error) {
	var sup string
	switch src.SupplyType {
	case B2B:
		sup = "B2B"
	case ExportLUT:
		sup = "EXPWOP"
	case ExportIGST:
		sup = "EXPWP"
	default:
		return nil, fmt.Errorf("%w (supply type %q)", ErrNotEligible, src.SupplyType)
	}
	export := src.SupplyType != B2B
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if !numberRE.MatchString(src.Number) {
		bad("document number %q cannot be used on an e-invoice: it must be at most 16 characters of A-Z, 0-9, '/' and '-' and not start with 0 (shorten the invoice prefix of the tax entity)", src.Number)
	}
	if src.IssuedAt.IsZero() {
		bad("the document has no issue date")
	}
	if c := src.Credit; c != nil {
		if !numberRE.MatchString(c.InvoiceNumber) {
			bad("the invoice number %q the credit note answers is not a valid document number", c.InvoiceNumber)
		}
		if c.InvoiceDate.IsZero() {
			bad("the credit note does not say when the invoice it answers was issued")
		} else if !src.IssuedAt.IsZero() && istDay(src.IssuedAt).Before(istDay(c.InvoiceDate)) {
			bad("the credit note is dated before the invoice it answers")
		}
	}

	// Seller.
	s := src.Seller
	if !gstinRE.MatchString(s.GSTIN) {
		bad("seller GSTIN %q is not a 15-character GSTIN", s.GSTIN)
	}
	if strings.TrimSpace(s.LegalName) == "" {
		bad("seller legal name is empty")
	}
	if !stateRE.MatchString(s.StateCode) {
		bad("seller state code %q is not a 2-digit code", s.StateCode)
	}
	sName, _ := tax.StateName(s.StateCode)
	sAddr := splitAddress(s, sName)
	for _, p := range sAddr.problems {
		bad("seller address: %s (edit the tax entity's address; write it like '1 Marine Drive, Mumbai 400020')", p)
	}
	seller := SellerDtls{Gstin: s.GSTIN, LglNm: clip(s.LegalName, 100), Addr1: sAddr.addr1, Addr2: sAddr.addr2, Loc: sAddr.loc, Pin: sAddr.pin,
		Stcd: s.StateCode, Ph: phone(s.Phone), Em: clip(s.Email, 100)}
	if tn := strings.TrimSpace(s.TradeName); tn != "" && tn != s.LegalName {
		seller.TrdNm = clip(tn, 100)
	}
	if len(seller.Em) < 6 {
		seller.Em = ""
	}

	// Buyer.
	b := src.Buyer
	var buyer BuyerDtls
	if strings.TrimSpace(b.LegalName) == "" {
		bad("buyer name is empty")
	}
	if export {
		cc := strings.ToUpper(strings.TrimSpace(b.Country))
		if !countryRE.MatchString(cc) || cc == "IN" {
			bad("the buyer's country %q must be a 2-letter code other than IN for an export", b.Country)
		}
		place := firstNonEmpty(b.CountryName, cc)
		addr := firstNonEmpty(collapse(b.Address), joinNonEmpty(", ", b.Line1, b.Line2, b.City), place)
		buyer = BuyerDtls{Gstin: "URP", LglNm: clip(b.LegalName, 100), Pos: "96", Addr1: clip(addr, 100), Loc: clip(firstNonEmpty(b.City, place, "Foreign"), 50),
			Pin: 999999, Stcd: "96"}
		if len([]rune(buyer.Loc)) < 3 {
			buyer.Loc = "Foreign"
		}
		if src.Currency == "" || !currencyRE.MatchString(src.Currency) {
			bad("an export needs the invoice currency (3 letters)")
		}
		if src.ForeignTotal <= 0 {
			bad("an export needs the invoice total in the foreign currency")
		}
	} else {
		if !gstinRE.MatchString(b.GSTIN) {
			bad("buyer GSTIN %q is required for a B2B invoice and must be 15 characters", b.GSTIN)
		}
		pos := firstNonEmpty(src.PlaceOfSupply, b.StateCode)
		if !stateRE.MatchString(pos) {
			bad("the place of supply %q is not a 2-digit state code", pos)
		}
		stcd := b.StateCode
		if gstinRE.MatchString(b.GSTIN) {
			stcd = b.GSTIN[:2]
		}
		if !stateRE.MatchString(stcd) {
			bad("the buyer's state code %q is not a 2-digit code", stcd)
		}
		bName, _ := tax.StateName(stcd)
		bAddr := splitAddress(b, bName)
		for _, p := range bAddr.problems {
			bad("buyer address: %s (the customer's billing address needs a line, a city and a 6-digit PIN code)", p)
		}
		buyer = BuyerDtls{Gstin: b.GSTIN, LglNm: clip(b.LegalName, 100), Pos: pos, Addr1: bAddr.addr1, Addr2: bAddr.addr2, Loc: bAddr.loc, Pin: bAddr.pin, Stcd: stcd}
	}

	// Lines.
	if len(src.Lines) == 0 {
		bad("the invoice has no lines")
	}
	var items []Item
	var assVal, cgst, sgst, igst, total int64
	for i, l := range src.Lines {
		n := i + 1
		if !sacRE.MatchString(l.SAC) {
			bad("line %d: SAC code %q must be 4 to 8 digits", n, l.SAC)
		}
		desc := collapse(l.Description)
		if len([]rune(desc)) < 3 {
			bad("line %d: the description must have at least 3 characters", n)
		}
		qty := l.Qty
		if qty <= 0 {
			qty = 1
		}
		rate := l.CGSTRateBP + l.SGSTRateBP
		if l.IGSTRateBP > 0 {
			rate = l.IGSTRateBP
		}
		lineTotal := l.Taxable + l.CGST + l.SGST + l.IGST
		items = append(items, Item{SlNo: strconv.Itoa(n), PrdDesc: clip(desc, 300), IsServc: "Y", HsnCd: l.SAC, Qty: json.Number(strconv.FormatInt(qty, 10)), Unit: "OTH",
			UnitPrice: unitPrice(l.Taxable, qty), TotAmt: paise(l.Taxable), AssAmt: paise(l.Taxable), GstRt: percent(rate),
			IgstAmt: paise(l.IGST), CgstAmt: paise(l.CGST), SgstAmt: paise(l.SGST), TotItemVal: paise(lineTotal)})
		assVal += l.Taxable
		cgst += l.CGST
		sgst += l.SGST
		igst += l.IGST
		total += lineTotal
	}
	if len(problems) > 0 {
		return nil, &Problems{List: problems}
	}

	doc := &Invoice{Version: Version,
		TranDtls:   TranDtls{TaxSch: "GST", SupTyp: sup, RegRev: "N", IgstOnIntra: "N"},
		DocDtls:    DocDtls{Typ: "INV", No: src.Number, Dt: src.IssuedAt.In(tax.IST).Format("02/01/2006")},
		SellerDtls: seller, BuyerDtls: buyer, ItemList: items,
		ValDtls: ValDtls{AssVal: paise(assVal), CgstVal: paise(cgst), SgstVal: paise(sgst), IgstVal: paise(igst), TotInvVal: paise(total)}}
	if c := src.Credit; c != nil {
		doc.DocDtls.Typ = "CRN"
		doc.RefDtls = &RefDtls{PrecDocDtls: []PrecDoc{{InvNo: c.InvoiceNumber, InvDt: c.InvoiceDate.In(tax.IST).Format("02/01/2006")}}}
	}
	if export {
		fc, err := money.FormatDecimal(src.ForeignTotal, src.Currency)
		if err != nil {
			return nil, &Problems{List: []string{fmt.Sprintf("the currency %q is not supported: %v", src.Currency, err)}}
		}
		doc.ValDtls.TotInvValFc = json.Number(fc)
		if !strings.Contains(fc, ".") {
			doc.ValDtls.TotInvValFc = json.Number(fc + ".00")
		}
		// TODO(spec): RefClm (claiming a refund of the IGST paid) is "N" for LUT
		// exports; for exports with payment of IGST the operator may claim it, but
		// Paymux has no setting for that, so "N" is sent.
		doc.ExpDtls = &ExpDtls{RefClm: "N", ForCur: src.Currency, CntCode: strings.ToUpper(strings.TrimSpace(b.Country))}
	}
	return doc, nil
}

func istDay(t time.Time) time.Time {
	y, m, d := t.In(tax.IST).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tax.IST)
}

func unitPrice(taxable, qty int64) json.Number {
	if qty == 1 {
		return paise(taxable)
	}
	// Three decimals, rounded half up: paise * 10 / qty.
	milli := (taxable*10*2 + qty) / (qty * 2)
	return json.Number(fmt.Sprintf("%d.%03d", milli/1000, milli%1000))
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func joinNonEmpty(sep string, s ...string) string {
	var out []string
	for _, v := range s {
		if v = collapse(v); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, sep)
}

// Marshal renders one document as indented JSON.
func Marshal(doc *Invoice) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MarshalBatch renders several documents as the JSON array the portal's bulk
// upload takes.
func MarshalBatch(docs []*Invoice) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if docs == nil {
		docs = []*Invoice{}
	}
	if err := enc.Encode(docs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
