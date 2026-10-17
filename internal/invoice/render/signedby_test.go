package render

import (
	"strings"
	"testing"
)

// A document that is going to be digitally signed says so, instead of only
// "computer-generated"; one that is not signed keeps the signatory note.
func TestSignedByReplacesTheComputerGeneratedLegend(t *testing.T) {
	d := intraB2C(t)
	plain, err := Render(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if text := pdfText(t, plain); !strings.Contains(text, "This is a computer-generated document") || strings.Contains(text, "Digitally signed") {
		t.Errorf("unsigned document text:\n%s", text)
	}
	d.SignedBy = "Test Signatory"
	signed, err := Render(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	text := pdfText(t, signed)
	if !strings.Contains(text, "Digitally signed by Test Signatory") || strings.Contains(text, "This is a computer-generated document") {
		t.Errorf("signed document text:\n%s", text)
	}
}
