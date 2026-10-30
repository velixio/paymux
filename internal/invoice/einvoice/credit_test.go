package einvoice_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/invoice/einvoice"
)

// creditOf is a credit note for half of src's first line.
func creditOf(src einvoice.Source, number string) einvoice.Source {
	c := src
	c.Credit = &einvoice.CreditRef{InvoiceNumber: src.Number, InvoiceDate: src.IssuedAt}
	c.Number = number
	c.IssuedAt = src.IssuedAt.AddDate(0, 0, 30)
	c.Lines = nil
	for _, l := range src.Lines {
		l.Taxable, l.CGST, l.SGST, l.IGST = l.Taxable/2, l.CGST/2, l.SGST/2, l.IGST/2
		c.Lines = append(c.Lines, l)
	}
	if c.SupplyType != einvoice.B2B {
		c.ForeignTotal /= 2
	}
	return c
}

func TestCreditNoteBuildsACRNDocumentThatRefersToTheInvoice(t *testing.T) {
	for name, inv := range map[string]einvoice.Source{"intra": intraB2B(), "inter": interB2B(), "export under LUT": exportLUT(), "export with IGST": exportIGST()} {
		raw := build(t, creditOf(inv, "CN/2627/000001"))
		if err := einvoice.Validate(raw); err != nil {
			t.Errorf("%s: %v\n%s", name, err, raw)
		}
		s := string(raw)
		wantDate := inv.IssuedAt.AddDate(0, 0, 30).In(ist).Format("02/01/2006")
		for _, want := range []string{`"Typ": "CRN"`, `"No": "CN/2627/000001"`, `"Dt": "` + wantDate + `"`, `"PrecDocDtls"`,
			`"InvNo": "` + inv.Number + `"`, `"InvDt": "` + inv.IssuedAt.In(ist).Format("02/01/2006") + `"`} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: missing %s in\n%s", name, want, s)
			}
		}
		// Amounts are positive, as the portal wants them on a credit note.
		if strings.Contains(s, `": -`) {
			t.Errorf("%s: negative amount in\n%s", name, s)
		}
	}
	// An invoice has no reference to a preceding document.
	if strings.Contains(string(build(t, intraB2B())), "RefDtls") {
		t.Error("an invoice carries RefDtls")
	}
}

func TestCreditNoteTotalsAreThoseOfTheCreditNote(t *testing.T) {
	raw := build(t, creditOf(intraB2B(), "CN/2627/000001"))
	for _, want := range []string{`"AssVal": 500.00`, `"CgstVal": 45.00`, `"SgstVal": 45.00`, `"TotInvVal": 590.00`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %s in\n%s", want, raw)
		}
	}
}

func TestCreditNoteNeedsTheInvoiceItAnswers(t *testing.T) {
	c := creditOf(intraB2B(), "CN/2627/000001")
	for name, mut := range map[string]func(*einvoice.Source){
		"no invoice number": func(s *einvoice.Source) { s.Credit.InvoiceNumber = "" },
		"no invoice date":   func(s *einvoice.Source) { s.Credit.InvoiceDate = time.Time{} },
		"bad invoice no":    func(s *einvoice.Source) { s.Credit.InvoiceNumber = "inv 1" },
		"credit before it":  func(s *einvoice.Source) { s.IssuedAt = s.Credit.InvoiceDate.AddDate(0, 0, -1) },
	} {
		s := c
		cr := *c.Credit
		s.Credit = &cr
		mut(&s)
		_, err := einvoice.Build(s)
		var p *einvoice.Problems
		if !errors.As(err, &p) {
			t.Errorf("%s: err = %v, want *Problems", name, err)
		}
	}
}
