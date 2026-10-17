// Package pades signs a finished PDF with a digital signature certificate
// (DSC): a detached CAdES signature (PAdES baseline B-B) added as an
// incremental update, so the bytes of the document are never touched and a
// reader can tell exactly what was signed.
//
// It exists for GST invoices: a digital signature on the PDF is the route the
// IT Act gives for signing an electronic invoice (CBIC circular 74/2018 proviso),
// next to the printed signatory note. The certificate comes as the PKCS#12
// file the certifying authority issues. Everything is the standard library
// plus golang.org/x/crypto/pkcs12 (already part of the module) to open the file.
//
// The signer understands the PDFs Paymux renders (fpdf: one classic
// cross-reference table, a flat page tree). Anything else is refused with an
// error rather than risk corrupting a document.
//
// What is not done: a signature timestamp (PAdES-T) and revocation data
// (PAdES-LT), which need a timestamp authority and the CA's OCSP responder;
// and nothing here was validated in Adobe Acrobat or against the Indian
// CCA trust list, only against OpenSSL and an independent parser in the tests.
package pades

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pkcs12"
)

// placeholderBytes is the room reserved for the CMS blob (hex-encoded in the
// file, so twice as many bytes). A signature with a certificate chain of a few
// kilobytes fits with plenty to spare.
const placeholderBytes = 10 * 1024

// Signer signs PDFs with one certificate.
type Signer struct {
	key   crypto.Signer
	cert  *x509.Certificate
	chain []*x509.Certificate
	now   func() time.Time
}

// Info describes the certificate of a Signer, for display.
type Info struct {
	Subject, Issuer     string
	NotBefore, NotAfter time.Time
	Fingerprint         string // hex SHA-256 of the certificate
}

// New opens a PKCS#12 file. It fails with a message an operator can act on if
// the password is wrong, the file is in a format the standard library cannot
// read (OpenSSL 3 encrypts with AES by default; re-export with -legacy), the
// key is not RSA or ECDSA, or the certificate is not valid now.
func New(p12 []byte, password string, now func() time.Time) (*Signer, error) {
	if now == nil {
		now = time.Now
	}
	blocks, err := pkcs12.ToPEM(p12, password)
	switch {
	case errors.Is(err, pkcs12.ErrIncorrectPassword):
		return nil, errors.New("pades: the PKCS#12 password is wrong")
	case err != nil:
		var ni pkcs12.NotImplementedError
		if errors.As(err, &ni) {
			return nil, fmt.Errorf("pades: this PKCS#12 file uses an encryption Paymux cannot read (%v); export it again with the legacy 3DES algorithms, for example: openssl pkcs12 -export -legacy -certpbe PBE-SHA1-3DES -keypbe PBE-SHA1-3DES -macalg sha1", ni)
		}
		return nil, fmt.Errorf("pades: reading the PKCS#12 file: %w", err)
	}
	var key crypto.Signer
	var certs []*x509.Certificate
	for _, b := range blocks {
		switch b.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("pades: a certificate in the file: %w", err)
			}
			certs = append(certs, c)
		case "PRIVATE KEY":
			if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
				key = k
			} else if k, err := x509.ParseECPrivateKey(b.Bytes); err == nil {
				key = k
			} else {
				return nil, errors.New("pades: the private key in the file is neither RSA nor ECDSA")
			}
		}
	}
	if key == nil {
		return nil, errors.New("pades: the PKCS#12 file holds no private key")
	}
	s := &Signer{key: key, now: now}
	for _, c := range certs {
		if publicKeyEqual(c.PublicKey, key.Public()) {
			s.cert = c
		} else {
			s.chain = append(s.chain, c)
		}
	}
	if s.cert == nil {
		return nil, errors.New("pades: the file holds no certificate for its private key")
	}
	switch s.cert.PublicKey.(type) {
	case *rsa.PublicKey, *ecdsa.PublicKey:
	default:
		return nil, errors.New("pades: only RSA and ECDSA certificates are supported")
	}
	if t := now(); t.Before(s.cert.NotBefore) {
		return nil, fmt.Errorf("pades: the certificate is not yet valid (from %s)", s.cert.NotBefore.UTC().Format("2006-01-02"))
	} else if t.After(s.cert.NotAfter) {
		return nil, fmt.Errorf("pades: the certificate expired on %s", s.cert.NotAfter.UTC().Format("2006-01-02"))
	}
	return s, nil
}

func publicKeyEqual(a, b any) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	if e, ok := a.(equaler); ok {
		return e.Equal(b)
	}
	return false
}

// Info describes the signing certificate.
func (s *Signer) Info() Info {
	sum := sha256.Sum256(s.cert.Raw)
	return Info{Subject: s.cert.Subject.String(), Issuer: s.cert.Issuer.String(), NotBefore: s.cert.NotBefore, NotAfter: s.cert.NotAfter,
		Fingerprint: hex.EncodeToString(sum[:])}
}

// Sign returns pdf with a signature appended. The input must be a PDF with one
// classic cross-reference table, no AcroForm and no annotations on its first
// page, which is what Paymux renders.
func (s *Signer) Sign(pdf []byte) ([]byte, error) {
	doc, err := parse(pdf)
	if err != nil {
		return nil, err
	}
	at := s.now().UTC()
	sigID, widgetID := doc.size, doc.size+1

	page := addBeforeClose(doc.page.body, fmt.Sprintf("/Annots [%d 0 R]", widgetID))
	catalog := addBeforeClose(doc.catalog.body, fmt.Sprintf("/AcroForm <</Fields [%d 0 R] /SigFlags 3>>", widgetID))

	var buf bytes.Buffer
	buf.Write(pdf)
	if !bytes.HasSuffix(pdf, []byte("\n")) {
		buf.WriteByte('\n')
	}
	offsets := map[int]int{}
	obj := func(num int, body string) {
		offsets[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", num, body)
	}
	obj(doc.page.num, page)
	obj(doc.catalog.num, catalog)

	// The signature dictionary, with the byte range and the contents left as
	// fixed-width placeholders that are patched once the digest is known.
	offsets[sigID] = buf.Len()
	fmt.Fprintf(&buf, "%d 0 obj\n<</Type /Sig /Filter /Adobe.PPKLite /SubFilter /ETSI.CAdES.detached /ByteRange [0 ", sigID)
	brPos := buf.Len()
	fmt.Fprintf(&buf, "%s %s %s] /Contents <", brPlaceholder, brPlaceholder, brPlaceholder)
	contentsStart := buf.Len() - 1 // the '<'
	buf.WriteString(strings.Repeat("0", 2*placeholderBytes))
	buf.WriteByte('>')
	contentsEnd := buf.Len()
	fmt.Fprintf(&buf, " /M (D:%s) /Name %s>>\nendobj\n", at.Format("20060102150405")+"Z", pdfString(s.cert.Subject.CommonName))

	obj(widgetID, fmt.Sprintf("<</Type /Annot /Subtype /Widget /FT /Sig /Rect [0 0 0 0] /F 132 /T (Signature1) /V %d 0 R /P %d 0 R>>", sigID, doc.pageNum))

	xrefAt := buf.Len()
	buf.WriteString("xref\n")
	nums := []int{doc.page.num, doc.catalog.num, sigID, widgetID}
	writeSubsections(&buf, nums, offsets)
	fmt.Fprintf(&buf, "trailer\n<</Size %d /Root %d 0 R%s /Prev %d>>\nstartxref\n%d\n%%%%EOF\n", widgetID+1, doc.rootNum, doc.infoRef(), doc.prev, xrefAt)

	out := buf.Bytes()
	br := fmt.Sprintf("%010d %010d %010d", contentsStart, contentsEnd, len(out)-contentsEnd)
	copy(out[brPos:], br)

	signed := make([]byte, 0, len(out))
	signed = append(signed, out[:contentsStart]...)
	signed = append(signed, out[contentsEnd:]...)
	blob, err := s.cms(signed, at)
	if err != nil {
		return nil, err
	}
	if len(blob) > placeholderBytes {
		return nil, fmt.Errorf("pades: the signature (%d bytes) does not fit the %d bytes reserved for it; the certificate chain is too long", len(blob), placeholderBytes)
	}
	hexBlob := hex.EncodeToString(blob)
	copy(out[contentsStart+1:], hexBlob)
	return out, nil
}

// brPlaceholder stands for one /ByteRange number; the real ones are written
// with the same width so no offset moves.
const brPlaceholder = "0000000000"

// writeSubsections writes the new cross-reference entries, one subsection for
// each run of consecutive object numbers.
func writeSubsections(buf *bytes.Buffer, nums []int, offsets map[int]int) {
	sorted := append([]int(nil), nums...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	for i := 0; i < len(sorted); {
		j := i + 1
		for j < len(sorted) && sorted[j] == sorted[j-1]+1 {
			j++
		}
		fmt.Fprintf(buf, "%d %d\n", sorted[i], j-i)
		for _, n := range sorted[i:j] {
			fmt.Fprintf(buf, "%010d 00000 n \n", offsets[n])
		}
		i = j
	}
}

// addBeforeClose inserts an entry before the final ">>" of a dictionary object.
func addBeforeClose(dict, entry string) string {
	i := strings.LastIndex(dict, ">>")
	return dict[:i] + " " + entry + dict[i:]
}

// pdfString renders s as a PDF string: literal when plain ASCII, else UTF-16BE.
func pdfString(s string) string {
	plain := true
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			plain = false
		}
	}
	if plain {
		return "(" + strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(s) + ")"
	}
	var b strings.Builder
	b.WriteString("<FEFF")
	for _, u := range encodeUTF16(s) {
		fmt.Fprintf(&b, "%04X", u)
	}
	b.WriteByte('>')
	return b.String()
}

func encodeUTF16(s string) []uint16 {
	var out []uint16
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		} else {
			out = append(out, uint16(r))
		}
	}
	return out
}

// ---- reading the document ----

type object struct {
	num  int
	body string // the dictionary text between "obj" and "endobj"
}

type document struct {
	size, rootNum, prev int
	info                string // "n 0 R" or ""
	catalog, page       object
	pageNum             int
}

func (d document) infoRef() string {
	if d.info == "" {
		return ""
	}
	return " /Info " + d.info
}

var (
	startxrefRE = regexp.MustCompile(`startxref\s+(\d+)\s+%%EOF\s*$`)
	sizeRE      = regexp.MustCompile(`/Size\s+(\d+)`)
	rootRE      = regexp.MustCompile(`/Root\s+(\d+)\s+0\s+R`)
	infoRE      = regexp.MustCompile(`/Info\s+(\d+\s+0\s+R)`)
	pagesRE     = regexp.MustCompile(`/Pages\s+(\d+)\s+0\s+R`)
	kidsRE      = regexp.MustCompile(`/Kids\s*\[\s*(\d+)\s+0\s+R`)
)

// parse reads just enough of the PDF: the cross-reference table, the trailer,
// the catalog and the first page.
func parse(pdf []byte) (document, error) {
	bad := func(format string, a ...any) (document, error) {
		return document{}, fmt.Errorf("pades: cannot sign this PDF: "+format, a...)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return bad("it is not a PDF")
	}
	m := startxrefRE.FindSubmatch(pdf)
	if m == nil {
		return bad("no startxref at the end")
	}
	prev, _ := strconv.Atoi(string(m[1]))
	if prev <= 0 || prev >= len(pdf) || !bytes.HasPrefix(pdf[prev:], []byte("xref")) {
		return bad("startxref does not point at a classic cross-reference table")
	}
	table := string(pdf[prev:])
	ti := strings.Index(table, "trailer")
	if ti < 0 {
		return bad("no trailer")
	}
	trailer := table[ti:]
	if strings.Contains(trailer, "/Prev") || strings.Contains(trailer, "/Encrypt") {
		return bad("it has several cross-reference sections or is encrypted")
	}
	sm, rm := sizeRE.FindStringSubmatch(trailer), rootRE.FindStringSubmatch(trailer)
	if sm == nil || rm == nil {
		return bad("the trailer has no /Size or /Root")
	}
	d := document{prev: prev}
	d.size, _ = strconv.Atoi(sm[1])
	d.rootNum, _ = strconv.Atoi(rm[1])
	if im := infoRE.FindStringSubmatch(trailer); im != nil {
		d.info = strings.Join(strings.Fields(im[1]), " ")
	}

	offsets, err := readXref(table[:ti])
	if err != nil {
		return bad("%v", err)
	}
	read := func(num int) (object, error) {
		off, ok := offsets[num]
		if !ok || off <= 0 || off >= len(pdf) {
			return object{}, fmt.Errorf("object %d is not in the cross-reference table", num)
		}
		rest := string(pdf[off:])
		head := fmt.Sprintf("%d 0 obj", num)
		if !strings.HasPrefix(rest, head) {
			return object{}, fmt.Errorf("object %d is not at its offset", num)
		}
		end := strings.Index(rest, "endobj")
		if end < 0 {
			return object{}, fmt.Errorf("object %d has no endobj", num)
		}
		return object{num: num, body: strings.TrimSpace(rest[len(head):end])}, nil
	}
	if d.catalog, err = read(d.rootNum); err != nil {
		return bad("%v", err)
	}
	if strings.Contains(d.catalog.body, "/AcroForm") {
		return bad("it already has an AcroForm (a signature or a form)")
	}
	pm := pagesRE.FindStringSubmatch(d.catalog.body)
	if pm == nil {
		return bad("the catalog has no /Pages")
	}
	pagesNum, _ := strconv.Atoi(pm[1])
	pages, err := read(pagesNum)
	if err != nil {
		return bad("%v", err)
	}
	km := kidsRE.FindStringSubmatch(pages.body)
	if km == nil {
		return bad("the page tree has no pages")
	}
	d.pageNum, _ = strconv.Atoi(km[1])
	if d.page, err = read(d.pageNum); err != nil {
		return bad("%v", err)
	}
	if !strings.Contains(d.page.body, "/Type /Page") || strings.Contains(d.page.body, "/Type /Pages") {
		return bad("the first kid of the page tree is not a page")
	}
	if strings.Contains(d.page.body, "/Annots") {
		return bad("its first page already has annotations")
	}
	if !strings.HasPrefix(d.page.body, "<<") || !strings.HasSuffix(d.page.body, ">>") ||
		!strings.HasPrefix(d.catalog.body, "<<") || !strings.HasSuffix(d.catalog.body, ">>") {
		return bad("the catalog or first page is not a plain dictionary")
	}
	return d, nil
}

// readXref reads the entries of a classic cross-reference table (the text
// from "xref" up to "trailer") into object number -> offset.
func readXref(table string) (map[int]int, error) {
	lines := strings.Split(strings.ReplaceAll(table, "\r", ""), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "xref" {
		return nil, errors.New("malformed cross-reference table")
	}
	out := map[int]int{}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		var start, n int
		if _, err := fmt.Sscanf(lines[i], "%d %d", &start, &n); err != nil {
			return nil, errors.New("malformed cross-reference subsection")
		}
		for k := 0; k < n; k++ {
			i++
			if i >= len(lines) {
				return nil, errors.New("truncated cross-reference table")
			}
			f := strings.Fields(lines[i])
			if len(f) != 3 {
				return nil, errors.New("malformed cross-reference entry")
			}
			if f[2] == "n" {
				off, err := strconv.Atoi(f[0])
				if err != nil {
					return nil, errors.New("malformed cross-reference offset")
				}
				out[start+k] = off
			}
		}
	}
	return out, nil
}
