// Package qrcode draws QR codes: it wraps the small pure-Go encoder rsc.io/qr
// and offers what the invoice pages and PDFs need, the modules and one SVG
// path. The e-invoice's signed QR code (a ~1 KB token) is the main user.
package qrcode

import (
	"errors"
	"strconv"
	"strings"

	"rsc.io/qr"
)

// quietZone is the empty border, in modules, a scanner needs around a code.
const quietZone = 4

// Code is an encoded QR code.
type Code struct {
	Size int // modules per side, without the quiet zone
	c    *qr.Code
}

// New encodes text at error correction level M (about 15% of the code can be
// damaged). Text that does not fit even the largest code is an error, and
// callers then show the text instead.
func New(text string) (*Code, error) {
	if text == "" {
		return nil, errors.New("qrcode: nothing to encode")
	}
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	return &Code{Size: c.Size, c: c}, nil
}

// Black reports whether the module at column x, row y is dark; modules outside
// the code are light.
func (c *Code) Black(x, y int) bool {
	if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
		return false
	}
	return c.c.Black(x, y)
}

// ViewBox is the side of the square an SVG of this code needs: the modules plus
// the quiet zone on each side.
func (c *Code) ViewBox() int { return c.Size + 2*quietZone }

// Path is the dark modules as one SVG path (for viewBox 0 0 ViewBox ViewBox):
// horizontal runs, each a 1-module-high rectangle.
func (c *Code) Path() string {
	var b strings.Builder
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; {
			if !c.Black(x, y) {
				x++
				continue
			}
			start := x
			for x < c.Size && c.Black(x, y) {
				x++
			}
			w := strconv.Itoa(x - start)
			b.WriteString("M" + strconv.Itoa(start+quietZone) + " " + strconv.Itoa(y+quietZone) + "h" + w + "v1h-" + w + "z")
		}
	}
	return b.String()
}
