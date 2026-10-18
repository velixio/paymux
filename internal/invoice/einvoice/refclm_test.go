package einvoice_test

import (
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/invoice/einvoice"
)

// RefClm says whether the exporter claims a refund of the IGST paid. Only an
// export with IGST paid has any to claim; under a LUT no tax was paid.
func TestRefundClaimFollowsTheSettingAndWhatWasPaid(t *testing.T) {
	cases := []struct {
		name    string
		src     einvoice.Source
		setting string
		want    string
	}{
		{"igst export, auto", exportIGST(), "auto", "Y"},
		{"igst export, unset is auto", exportIGST(), "", "Y"},
		{"igst export, forced N", exportIGST(), "N", "N"},
		{"igst export, forced Y", exportIGST(), "Y", "Y"},
		{"lut export, auto", exportLUT(), "auto", "N"},
		{"lut export, forced Y has nothing to claim", exportLUT(), "Y", "N"},
		{"lut export, forced N", exportLUT(), "N", "N"},
	}
	for _, c := range cases {
		c.src.IGSTRefundClaim = c.setting
		raw := build(t, c.src)
		if want := `"RefClm": "` + c.want + `"`; !strings.Contains(string(raw), want) {
			t.Errorf("%s: want %s in\n%s", c.name, want, raw)
		}
		if err := einvoice.Validate(raw); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// A value that is none of the three is refused rather than guessed at.
	bad := exportIGST()
	bad.IGSTRefundClaim = "maybe"
	if _, err := einvoice.Build(bad); err == nil {
		t.Error("an unknown refund claim setting was accepted")
	}
	// Domestic invoices have no export details.
	d := intraB2B()
	d.IGSTRefundClaim = "Y"
	if strings.Contains(string(build(t, d)), "RefClm") {
		t.Error("a domestic invoice carries RefClm")
	}
}
