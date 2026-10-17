package render

import (
	"bytes"
	"compress/zlib"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// A minimal PDF text extractor for the files this package writes: it inflates
// the streams, maps each font's 2-byte codes through its ToUnicode CMap, and
// returns the text shown by the Tj operators in order. (General-purpose
// extractors ignore the CMap and mangle non-Latin-1 text such as the rupee
// sign, which is exactly what these tests must check.)

var (
	objRE    = regexp.MustCompile(`(?s)(\d+) 0 obj\s*(.*?)endobj`)
	streamRE = regexp.MustCompile(`(?s)^(.*?)stream\r?\n(.*)\r?\nendstream`)
	fontRes  = regexp.MustCompile(`/(F[0-9a-f]{20,}) (\d+) 0 R`)
	toUni    = regexp.MustCompile(`/ToUnicode (\d+) 0 R`)
	tfRE     = regexp.MustCompile(`/(F[0-9a-f]{20,}) [0-9.]+ Tf`)
	tjRE     = regexp.MustCompile(`(?s)\(((?:\\.|[^\\)])*)\)\s*Tj`)
	bfchar   = regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>`)
	bfrange  = regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>`)
)

type pdfObj struct {
	dict   string
	stream []byte
}

func parseObjs(t *testing.T, b []byte) map[int]pdfObj {
	t.Helper()
	out := map[int]pdfObj{}
	for _, m := range objRE.FindAllSubmatch(b, -1) {
		n, _ := strconv.Atoi(string(m[1]))
		o := pdfObj{dict: string(m[2])}
		if sm := streamRE.FindSubmatch(m[2]); sm != nil {
			o.dict = string(sm[1])
			raw := sm[2]
			if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
				if inflated, err := io.ReadAll(zr); err == nil {
					raw = inflated
				}
			}
			o.stream = raw
		}
		out[n] = o
	}
	return out
}

func hexRune(h string) rune {
	v, _ := strconv.ParseUint(h, 16, 32)
	if len(h) > 4 { // UTF-16 surrogate pair
		u := utf16.Decode([]uint16{uint16(v >> 16), uint16(v)})
		return u[0]
	}
	return rune(v)
}

func parseCMap(s string) map[rune]rune {
	m := map[rune]rune{}
	for _, sec := range strings.Split(s, "beginbfrange")[1:] {
		sec, _, _ = strings.Cut(sec, "endbfrange")
		for _, r := range bfrange.FindAllStringSubmatch(sec, -1) {
			lo, hi, dst := hexRune(r[1]), hexRune(r[2]), hexRune(r[3])
			for c := lo; c <= hi; c++ {
				m[c] = dst + (c - lo)
			}
		}
	}
	for _, sec := range strings.Split(s, "beginbfchar")[1:] {
		sec, _, _ = strings.Cut(sec, "endbfchar")
		for _, r := range bfchar.FindAllStringSubmatch(sec, -1) {
			m[hexRune(r[1])] = hexRune(r[2])
		}
	}
	return m
}

func unescape(s []byte) []byte {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'r':
				out = append(out, '\r')
			case 'n':
				out = append(out, '\n')
			default:
				out = append(out, s[i])
			}
			continue
		}
		out = append(out, s[i])
	}
	return out
}

// pdfText returns the whitespace-normalised text of every page.
func pdfText(t *testing.T, b []byte) string {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatal("not a PDF")
	}
	objs := parseObjs(t, b)
	fontMaps := map[string]map[rune]rune{} // resource name -> code -> rune
	for _, o := range objs {
		for _, f := range fontRes.FindAllStringSubmatch(o.dict, -1) {
			n, _ := strconv.Atoi(f[2])
			fo, ok := objs[n]
			if !ok {
				continue
			}
			if u := toUni.FindStringSubmatch(fo.dict); u != nil {
				un, _ := strconv.Atoi(u[1])
				fontMaps[f[1]] = parseCMap(string(objs[un].stream))
			}
		}
	}
	var out []string
	// Content streams in object order (fpdf writes pages in order).
	for n := 1; n < len(objs)+2; n++ {
		o, ok := objs[n]
		if !ok || !bytes.Contains(o.stream, []byte(" Tf")) {
			continue
		}
		var cur map[rune]rune
		rest := o.stream
		for len(rest) > 0 {
			ti, tj := tfRE.FindSubmatchIndex(rest), tjRE.FindSubmatchIndex(rest)
			if tj == nil {
				break
			}
			if ti != nil && ti[0] < tj[0] {
				cur = fontMaps[string(rest[ti[2]:ti[3]])]
				rest = rest[ti[1]:]
				continue
			}
			raw := unescape(rest[tj[2]:tj[3]])
			var sb strings.Builder
			for i := 0; i+1 < len(raw); i += 2 {
				code := rune(raw[i])<<8 | rune(raw[i+1])
				if r, ok := cur[code]; ok {
					sb.WriteRune(r)
				} else {
					sb.WriteRune(code) // identity fallback
				}
			}
			out = append(out, sb.String())
			rest = rest[tj[1]:]
		}
	}
	return strings.Join(strings.Fields(strings.Join(out, " ")), " ")
}
