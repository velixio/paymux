package invoice

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/tax"
	"github.com/paymux/paymux/pkg/connector/money"
)

// B2CLThresholdPaise is the built-in value of the b2cl_threshold setting: the
// invoice value above which an inter-state B2C invoice goes to the B2CL sheet
// (Rs 1,00,000, Rule 59(4) as amended by Notification 12/2024).
const B2CLThresholdPaise int64 = 10_000_000

// ReportFile is one CSV of a monthly report.
type ReportFile struct {
	Name string
	CSV  []byte
}

// ReportSet is the monthly GST report of one tax entity.
type ReportSet struct {
	Entity store.TaxEntity
	Month  string // YYYY-MM
	Files  []ReportFile
}

// Write writes every file into dir (created if needed).
func (r ReportSet) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range r.Files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), f.CSV, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// monthRange returns [first day 00:00 IST, first day of the next month).
func monthRange(month string) (from, to time.Time, err error) {
	t, err := time.ParseInLocation("2006-01", month, tax.IST)
	if err != nil {
		return from, to, invalid("month %q must be YYYY-MM", month)
	}
	return t.UTC(), t.AddDate(0, 1, 0).UTC(), nil
}

type csvTable struct {
	name string
	rows [][]string
}

func (t *csvTable) add(cols ...string) { t.rows = append(t.rows, cols) }

func (t csvTable) file() (ReportFile, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(t.rows); err != nil {
		return ReportFile{}, err
	}
	return ReportFile{Name: t.name, CSV: buf.Bytes()}, nil
}

// rupees formats paise as a plain decimal ("1180.00"); blank for pending.
func rupees(paise int64, ok bool) string {
	if !ok {
		return ""
	}
	return minorStr(paise, "INR")
}

// minorStr formats minor units as a plain decimal; negatives (credit notes in
// the register) get a leading minus.
func minorStr(v int64, cur string) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	s, err := money.FormatDecimal(v, cur)
	if err != nil {
		return fmt.Sprint(v)
	}
	return sign + s
}

func istDate(t time.Time) string { return t.In(tax.IST).Format("2006-01-02") }

func rateStr(bp int) string { return minorStr(int64(bp), "INR") } // 1800 -> "18.00"

// doc is an invoice or credit note flattened for the sheets. Sign is -1 for
// credit notes in the sales register.
type doc struct {
	isCN                       bool
	number, date, cur          string
	invoiceNumber, invoiceDate string
	supply, pos                string
	interState                 bool
	rcp                        Recipient
	sac                        string
	rateBP                     int
	fx, fxSource, lut          string
	reason                     string
	flags                      []string
	amt                        tax.Amounts // document currency
	inr                        tax.Amounts // paise
	inrOK                      bool
	originB2CL                 bool // credit notes: the invoice was a B2CL invoice
	seriesPrefix, fiscalYear   string
	b2clThreshold              int64 // paise; the b2cl_threshold setting
}

func (d doc) class() tax.Classification {
	return tax.Classification{SupplyType: tax.SupplyType(d.supply), PlaceOfSupply: d.pos, InterState: d.interState}
}

func (d doc) b2cl() bool { return d.inrOK && tax.IsB2CL(d.class(), d.inr.Total, d.b2clThreshold) }

// Reports builds the monthly GST report CSVs of a tax entity: sales register,
// B2B, B2CL, B2CS, EXP, CDNR, CDNUR, SAC summary, documents issued and
// exceptions, then the settlement report and the provider fee input-tax-credit
// register. month is YYYY-MM in IST. Foreign-currency documents count in INR
// at their invoice's FX rate; those still fx_pending appear with blank INR
// values and in the exceptions sheet.
func (s *Service) Reports(ctx context.Context, taxEntityID, month string) (ReportSet, error) {
	from, to, err := monthRange(month)
	if err != nil {
		return ReportSet{}, err
	}
	var (
		ent     store.TaxEntity
		invs    []store.Invoice
		cns     []store.CreditNote
		flagged []store.RefundFlag
		fees    []store.SettlementFees
		origin  = map[string]store.Invoice{}
	)
	err = s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if ent, err = store.GetTaxEntity(ctx, q, taxEntityID); err != nil {
			return err
		}
		if invs, err = store.InvoicesInRange(ctx, q, ent.ID, from, to); err != nil {
			return err
		}
		if cns, err = store.CreditNotesInRange(ctx, q, ent.ID, from, to); err != nil {
			return err
		}
		for _, i := range invs {
			origin[i.ID] = i
		}
		for _, c := range cns {
			if _, ok := origin[c.InvoiceID]; !ok {
				i, err := store.GetInvoice(ctx, q, c.InvoiceID)
				if err != nil {
					return err
				}
				origin[i.ID] = i
			}
		}
		if flagged, err = store.RefundFlagsInRange(ctx, q, ent.ID, from, to); err != nil {
			return err
		}
		fees, err = store.SettlementFeesForEntity(ctx, q, ent.ID, from, to)
		return err
	})
	if err != nil {
		return ReportSet{}, err
	}

	b2cl, err := s.b2clThreshold(ctx, ent.OrgID)
	if err != nil {
		return ReportSet{}, err
	}
	var docs []doc
	var invDocs, cnDocs []doc
	for _, i := range invs {
		d, err := invoiceDoc(i, b2cl)
		if err != nil {
			return ReportSet{}, err
		}
		docs, invDocs = append(docs, d), append(invDocs, d)
	}
	for _, c := range cns {
		d, err := creditNoteDoc(c, origin[c.InvoiceID], b2cl)
		if err != nil {
			return ReportSet{}, err
		}
		docs, cnDocs = append(docs, d), append(cnDocs, d)
	}
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].date != docs[j].date {
			return docs[i].date < docs[j].date
		}
		return docs[i].number < docs[j].number
	})

	tables := []csvTable{
		salesRegister(docs),
		b2bSheet(invDocs),
		b2clSheet(invDocs),
		b2csSheet(invDocs, cnDocs),
		expSheet(invDocs),
		cdnSheet("cdnr.csv", cnDocs, func(d doc) bool { return d.supply == string(tax.B2B) }),
		cdnSheet("cdnur.csv", cnDocs, func(d doc) bool {
			// B2CS credit notes are netted in the B2CS sheet instead.
			return d.supply != string(tax.B2B) && !(d.supply == string(tax.B2C) && !d.originB2CL)
		}),
		sacSummary(invDocs, cnDocs),
		documentsSheet(invs, cns),
		exceptionsSheet(invDocs, flagged, istDate),
		settlementsSheet(fees),
		itcSheet(ent, fees),
	}
	set := ReportSet{Entity: ent, Month: month}
	for _, t := range tables {
		f, err := t.file()
		if err != nil {
			return ReportSet{}, err
		}
		set.Files = append(set.Files, f)
	}
	return set, nil
}

func invoiceDoc(i store.Invoice, b2cl int64) (doc, error) {
	var rcp Recipient
	if err := json.Unmarshal(i.Recipient, &rcp); err != nil {
		return doc{}, err
	}
	var lines []Line
	if err := json.Unmarshal(i.Lines, &lines); err != nil {
		return doc{}, err
	}
	d := doc{number: i.Number, date: istDate(i.IssuedAt), cur: i.Currency, supply: i.SupplyType, pos: i.PlaceOfSupply,
		interState: i.InterState, rcp: rcp, rateBP: i.GSTRateBP, fx: i.FXRate, fxSource: i.FXSource, lut: i.LUTNumber,
		flags: i.Flags, amt: amountsOf(i.Taxable, i.CGST, i.SGST, i.IGST, i.Total), seriesPrefix: i.SeriesPrefix, fiscalYear: i.FiscalYear,
		b2clThreshold: b2cl}
	if len(lines) > 0 {
		d.sac = lines[0].SAC
	}
	var err error
	d.inr, d.inrOK, err = inrAmounts(d.amt, i.Currency, i.FXRate)
	return d, err
}

func creditNoteDoc(c store.CreditNote, inv store.Invoice, b2cl int64) (doc, error) {
	d, err := invoiceDoc(inv, b2cl)
	if err != nil {
		return doc{}, err
	}
	d.originB2CL = d.b2cl()
	d.isCN, d.number, d.date, d.reason, d.flags = true, c.Number, istDate(c.IssuedAt), c.Reason, c.Flags
	d.invoiceNumber, d.invoiceDate = inv.Number, istDate(inv.IssuedAt)
	d.seriesPrefix, d.fiscalYear = c.SeriesPrefix, c.FiscalYear
	d.amt = amountsOf(c.Taxable, c.CGST, c.SGST, c.IGST, c.Total)
	d.inr, d.inrOK, err = inrAmounts(d.amt, c.Currency, inv.FXRate)
	return d, err
}

func salesRegister(docs []doc) csvTable {
	t := csvTable{name: "sales_register.csv"}
	t.add("doc_type", "number", "date", "against_invoice", "supply_type", "place_of_supply", "recipient", "recipient_gstin", "sac",
		"currency", "taxable", "cgst", "sgst", "igst", "total", "fx_rate_inr", "taxable_inr", "cgst_inr", "sgst_inr", "igst_inr",
		"total_inr", "flags")
	for _, d := range docs {
		sign := int64(1)
		typ := "invoice"
		if d.isCN {
			sign, typ = -1, "credit_note"
		}
		t.add(typ, d.number, d.date, d.invoiceNumber, d.supply, d.pos, d.rcp.Name, d.rcp.GSTIN, d.sac, d.cur,
			minorStr(sign*d.amt.Taxable, d.cur), minorStr(sign*d.amt.CGST, d.cur), minorStr(sign*d.amt.SGST, d.cur),
			minorStr(sign*d.amt.IGST, d.cur), minorStr(sign*d.amt.Total, d.cur), d.fx,
			rupees(sign*d.inr.Taxable, d.inrOK), rupees(sign*d.inr.CGST, d.inrOK), rupees(sign*d.inr.SGST, d.inrOK),
			rupees(sign*d.inr.IGST, d.inrOK), rupees(sign*d.inr.Total, d.inrOK), strings.Join(d.flags, " "))
	}
	return t
}

func b2bSheet(invs []doc) csvTable {
	t := csvTable{name: "b2b.csv"}
	t.add("gstin", "receiver_name", "invoice_number", "invoice_date", "invoice_value", "place_of_supply", "rate", "taxable_value", "igst", "cgst", "sgst")
	for _, d := range invs {
		if d.supply == string(tax.B2B) && d.inrOK {
			t.add(d.rcp.GSTIN, d.rcp.Name, d.number, d.date, rupees(d.inr.Total, true), d.pos, rateStr(d.rateBP),
				rupees(d.inr.Taxable, true), rupees(d.inr.IGST, true), rupees(d.inr.CGST, true), rupees(d.inr.SGST, true))
		}
	}
	return t
}

func b2clSheet(invs []doc) csvTable {
	t := csvTable{name: "b2cl.csv"}
	t.add("invoice_number", "invoice_date", "invoice_value", "place_of_supply", "rate", "taxable_value", "igst")
	for _, d := range invs {
		if d.b2cl() {
			t.add(d.number, d.date, rupees(d.inr.Total, true), d.pos, rateStr(d.rateBP), rupees(d.inr.Taxable, true), rupees(d.inr.IGST, true))
		}
	}
	return t
}

func b2csSheet(invs, cns []doc) csvTable {
	type key struct {
		pos  string
		rate int
	}
	sums := map[key]tax.Amounts{}
	add := func(d doc, sign int64) {
		k := key{d.pos, d.rateBP}
		a := sums[k]
		sums[k] = a.Add(tax.Amounts{Taxable: sign * d.inr.Taxable, CGST: sign * d.inr.CGST, SGST: sign * d.inr.SGST,
			IGST: sign * d.inr.IGST, Total: sign * d.inr.Total})
	}
	for _, d := range invs {
		if d.supply == string(tax.B2C) && d.inrOK && !d.b2cl() {
			add(d, 1)
		}
	}
	for _, d := range cns {
		if d.supply == string(tax.B2C) && d.inrOK && !d.originB2CL {
			add(d, -1)
		}
	}
	keys := make([]key, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pos != keys[j].pos {
			return keys[i].pos < keys[j].pos
		}
		return keys[i].rate < keys[j].rate
	})
	t := csvTable{name: "b2cs.csv"}
	t.add("place_of_supply", "rate", "taxable_value", "igst", "cgst", "sgst")
	for _, k := range keys {
		a := sums[k]
		t.add(k.pos, rateStr(k.rate), rupees(a.Taxable, true), rupees(a.IGST, true), rupees(a.CGST, true), rupees(a.SGST, true))
	}
	return t
}

func expSheet(invs []doc) csvTable {
	t := csvTable{name: "exp.csv"}
	t.add("export_type", "invoice_number", "invoice_date", "invoice_value", "rate", "taxable_value", "igst", "currency", "foreign_value",
		"fx_rate_inr", "lut_number", "destination_country")
	for _, d := range invs {
		if d.supply != string(tax.ExportLUT) && d.supply != string(tax.ExportIGST) {
			continue
		}
		typ := "WOPAY"
		if d.supply == string(tax.ExportIGST) {
			typ = "WPAY"
		}
		t.add(typ, d.number, d.date, rupees(d.inr.Total, d.inrOK), rateStr(d.rateBP), rupees(d.inr.Taxable, d.inrOK),
			rupees(d.inr.IGST, d.inrOK), d.cur, minorStr(d.amt.Total, d.cur), d.fx, d.lut, d.rcp.Country)
	}
	return t
}

func cdnSheet(name string, cns []doc, include func(doc) bool) csvTable {
	t := csvTable{name: name}
	t.add("gstin", "receiver_name", "note_number", "note_date", "against_invoice", "invoice_date", "reason", "place_of_supply", "rate",
		"taxable_value", "igst", "cgst", "sgst", "note_value")
	for _, d := range cns {
		if include(d) {
			t.add(d.rcp.GSTIN, d.rcp.Name, d.number, d.date, d.invoiceNumber, d.invoiceDate, d.reason, d.pos, rateStr(d.rateBP),
				rupees(d.inr.Taxable, d.inrOK), rupees(d.inr.IGST, d.inrOK), rupees(d.inr.CGST, d.inrOK), rupees(d.inr.SGST, d.inrOK),
				rupees(d.inr.Total, d.inrOK))
		}
	}
	return t
}

func sacSummary(invs, cns []doc) csvTable {
	type row struct {
		n int
		a tax.Amounts
	}
	sums := map[string]*row{}
	add := func(d doc, sign int64, count int) {
		if !d.inrOK {
			return
		}
		r := sums[d.sac]
		if r == nil {
			r = &row{}
			sums[d.sac] = r
		}
		r.n += count
		r.a = r.a.Add(tax.Amounts{Taxable: sign * d.inr.Taxable, CGST: sign * d.inr.CGST, SGST: sign * d.inr.SGST,
			IGST: sign * d.inr.IGST, Total: sign * d.inr.Total})
	}
	for _, d := range invs {
		add(d, 1, 1)
	}
	for _, d := range cns {
		add(d, -1, 0)
	}
	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t := csvTable{name: "sac_summary.csv"}
	t.add("sac", "uqc", "documents", "taxable_value", "igst", "cgst", "sgst", "total_value")
	for _, k := range keys {
		r := sums[k]
		t.add(k, "OTH", fmt.Sprint(r.n), rupees(r.a.Taxable, true), rupees(r.a.IGST, true), rupees(r.a.CGST, true),
			rupees(r.a.SGST, true), rupees(r.a.Total, true))
	}
	return t
}

func documentsSheet(invs []store.Invoice, cns []store.CreditNote) csvTable {
	type key struct{ kind, prefix, fy string }
	type span struct {
		first, last string
		n           int
	}
	spans := map[key]*span{}
	note := func(k key, number string) {
		s := spans[k]
		if s == nil {
			s = &span{first: number, last: number}
			spans[k] = s
		}
		if number < s.first {
			s.first = number
		}
		if number > s.last {
			s.last = number
		}
		s.n++
	}
	for _, i := range invs {
		note(key{"invoice", i.SeriesPrefix, i.FiscalYear}, i.Number)
	}
	for _, c := range cns {
		note(key{"credit_note", c.SeriesPrefix, c.FiscalYear}, c.Number)
	}
	keys := make([]key, 0, len(spans))
	for k := range spans {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.prefix != b.prefix {
			return a.prefix < b.prefix
		}
		return a.fy < b.fy
	})
	t := csvTable{name: "documents_issued.csv"}
	t.add("kind", "series_prefix", "fiscal_year", "first_number", "last_number", "issued", "cancelled")
	for _, k := range keys {
		s := spans[k]
		t.add(k.kind, k.prefix, tax.FiscalYearLabel(k.fy), s.first, s.last, fmt.Sprint(s.n), "0")
	}
	return t
}

func exceptionsSheet(invs []doc, flagged []store.RefundFlag, date func(time.Time) string) csvTable {
	t := csvTable{name: "exceptions.csv"}
	t.add("flag", "document", "date")
	for _, d := range invs {
		for _, f := range d.flags {
			t.add(f, d.number, d.date)
		}
	}
	for _, f := range flagged {
		t.add(f.Flag, "refund:"+f.RefundID, date(f.CreatedAt))
	}
	return t
}
