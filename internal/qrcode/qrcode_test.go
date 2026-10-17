package qrcode_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/internal/qrcode"
)

// a token about the size of the portal's signed QR code
func token(n int) string {
	return strings.Repeat("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.", n/30+1)[:n]
}

func TestEncodeMakesAScannableSquareWithFinderPatterns(t *testing.T) {
	c, err := qrcode.New(token(1100))
	if err != nil {
		t.Fatal(err)
	}
	// A QR code of version v is 17+4v modules wide.
	if c.Size < 21 || (c.Size-17)%4 != 0 {
		t.Fatalf("size %d is not a QR version", c.Size)
	}
	// The three finder patterns: a 7x7 ring with a 3x3 centre, in three corners.
	finder := func(x0, y0 int) {
		for dy := 0; dy < 7; dy++ {
			for dx := 0; dx < 7; dx++ {
				ring := dx == 0 || dx == 6 || dy == 0 || dy == 6
				centre := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
				if want := ring || centre; c.Black(x0+dx, y0+dy) != want {
					t.Fatalf("finder at (%d,%d): module (%d,%d) = %v, want %v", x0, y0, dx, dy, !want, want)
				}
			}
		}
	}
	finder(0, 0)
	finder(c.Size-7, 0)
	finder(0, c.Size-7)
	if c.Black(-1, 0) || c.Black(0, c.Size) {
		t.Error("outside the code is white")
	}
}

func TestEncodeIsDeterministicAndDistinguishesInputs(t *testing.T) {
	a1, _ := qrcode.New("alpha")
	a2, _ := qrcode.New("alpha")
	b, _ := qrcode.New("bravo")
	if a1.Path() != a2.Path() {
		t.Error("the same text gave different codes")
	}
	if a1.Path() == b.Path() {
		t.Error("different text gave the same code")
	}
}

func TestTextThatDoesNotFitIsAnError(t *testing.T) {
	if _, err := qrcode.New(strings.Repeat("A", 5000)); err == nil {
		t.Error("5000 bytes cannot fit a QR code at this error correction level")
	}
	if _, err := qrcode.New(""); err == nil {
		t.Error("an empty code is not useful")
	}
}

func TestPathIsASingleSVGPathInsideTheQuietZone(t *testing.T) {
	c, err := qrcode.New("hello")
	if err != nil {
		t.Fatal(err)
	}
	p := c.Path()
	if !regexp.MustCompile(`^(M[0-9]+ [0-9]+h[0-9]+v1h-[0-9]+z)+$`).MatchString(p) {
		t.Fatalf("path %q is not a run of rectangles", p[:min(80, len(p))])
	}
	// Every number stays inside the viewbox (modules plus a 4-module quiet zone each side).
	for _, m := range regexp.MustCompile(`[0-9]+`).FindAllString(p, -1) {
		var n int
		for _, ch := range m {
			n = n*10 + int(ch-'0')
		}
		if n > c.ViewBox() {
			t.Fatalf("coordinate %d outside the viewbox %d", n, c.ViewBox())
		}
	}
	if c.ViewBox() != c.Size+8 {
		t.Errorf("viewbox %d, want size %d + 8", c.ViewBox(), c.Size)
	}
}
