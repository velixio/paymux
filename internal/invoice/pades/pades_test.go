package pades_test

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/paymux/paymux/internal/invoice/pades"
)

const password = "test-dsc-pass"

var signedAt = time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newSigner(t *testing.T, name string) *pades.Signer {
	t.Helper()
	s, err := pades.New(fixture(t, name), password, func() time.Time { return signedAt })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// samplePDF is what the invoice renderer produces: a classic-xref PDF from fpdf.
func samplePDF(t *testing.T, pages int) []byte {
	t.Helper()
	p := fpdf.New("P", "mm", "A4", "")
	p.SetCreationDate(signedAt)
	p.SetFont("Helvetica", "", 12)
	for i := 0; i < pages; i++ {
		p.AddPage()
		p.Cell(40, 10, fmt.Sprintf("Tax invoice, page %d", i+1))
	}
	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var byteRangeRE = regexp.MustCompile(`/ByteRange \[(\d+) (\d+) (\d+) (\d+)\]`)

// signature extracts the signed bytes and the CMS blob of a signed PDF,
// checking the byte range covers the file but the signature.
func signature(t *testing.T, pdf []byte) (signed, cms []byte) {
	t.Helper()
	m := byteRangeRE.FindSubmatch(pdf)
	if m == nil {
		t.Fatal("no /ByteRange")
	}
	var br [4]int
	for i := range br {
		br[i], _ = strconv.Atoi(string(m[i+1]))
	}
	if br[0] != 0 || br[2] != br[1]+(br[2]-br[1]) || br[2]+br[3] != len(pdf) {
		t.Fatalf("byte range %v does not cover a %d-byte file", br, len(pdf))
	}
	if pdf[br[1]] != '<' || pdf[br[2]-1] != '>' {
		t.Fatalf("the gap %d..%d is not the hex /Contents string", br[1], br[2])
	}
	raw, err := hex.DecodeString(string(pdf[br[1]+1 : br[2]-1]))
	if err != nil {
		t.Fatalf("contents: %v", err)
	}
	return append(append([]byte{}, pdf[:br[1]]...), pdf[br[2]:]...), raw
}

type algID struct {
	Algorithm asn1.ObjectIdentifier
	Params    asn1.RawValue `asn1:"optional"`
}

type contentInfo struct {
	Type    asn1.ObjectIdentifier
	Content asn1.RawValue `asn1:"explicit,tag:0"`
}

type signedData struct {
	Version int
	Digests asn1.RawValue `asn1:"set"`
	Encap   asn1.RawValue
	Certs   asn1.RawValue `asn1:"optional,tag:0"`
	Signers []signerInfo  `asn1:"set"`
}

type signerInfo struct {
	Version int
	SID     asn1.RawValue
	Digest  algID
	Attrs   asn1.RawValue `asn1:"tag:0"`
	SigAlg  algID
	Sig     []byte
}

type attribute struct {
	Type   asn1.ObjectIdentifier
	Values asn1.RawValue `asn1:"set"`
}

var (
	oidMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidContentType   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidSigningTime   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 5}
	oidSigningCertV2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
)

// verifyCMS checks the detached CMS signature the way a validator does: the
// message digest attribute is the digest of the signed bytes, and the
// signature is over the DER-encoded signed attributes, by the embedded
// certificate. It returns that certificate.
func verifyCMS(t *testing.T, signed, blob []byte) *x509.Certificate {
	t.Helper()
	var ci contentInfo
	if _, err := asn1.Unmarshal(blob, &ci); err != nil || !ci.Type.Equal(oidSignedData) {
		t.Fatalf("not CMS SignedData: %v", err)
	}
	var sd signedData
	if _, err := asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil {
		t.Fatalf("SignedData: %v", err)
	}
	if len(sd.Signers) != 1 {
		t.Fatalf("%d signers", len(sd.Signers))
	}
	cert, err := x509.ParseCertificate(sd.Certs.Bytes)
	if err != nil {
		// Several certificates: the first is the signer's.
		var first asn1.RawValue
		if _, err2 := asn1.Unmarshal(sd.Certs.Bytes, &first); err2 != nil {
			t.Fatalf("certificates: %v", err)
		}
		if cert, err = x509.ParseCertificate(first.FullBytes); err != nil {
			t.Fatalf("certificate: %v", err)
		}
	}
	si := sd.Signers[0]
	rest := si.Attrs.Bytes
	have := map[string]attribute{}
	for len(rest) > 0 {
		var a attribute
		var err error
		if rest, err = asn1.Unmarshal(rest, &a); err != nil {
			t.Fatalf("attribute: %v", err)
		}
		have[a.Type.String()] = a
	}
	for _, oid := range []asn1.ObjectIdentifier{oidContentType, oidMessageDigest, oidSigningTime, oidSigningCertV2} {
		if _, ok := have[oid.String()]; !ok {
			t.Errorf("signed attribute %s missing", oid)
		}
	}
	var md []byte
	if _, err := asn1.Unmarshal(have[oidMessageDigest.String()].Values.Bytes, &md); err != nil {
		t.Fatalf("messageDigest: %v", err)
	}
	sum := sha256.Sum256(signed)
	if !bytes.Equal(md, sum[:]) {
		t.Error("the message digest is not the digest of the signed byte ranges")
	}
	set, _ := asn1.Marshal(asn1.RawValue{Tag: asn1.TagSet, IsCompound: true, Bytes: si.Attrs.Bytes})
	dig := sha256.Sum256(set)
	switch pub := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, dig[:], si.Sig); err != nil {
			t.Errorf("RSA signature: %v", err)
		}
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(pub, dig[:], si.Sig) {
			t.Error("ECDSA signature does not verify")
		}
	default:
		t.Fatalf("key type %T", pub)
	}
	// The certificate certified by the signer info is the one embedded.
	var sid struct {
		Issuer asn1.RawValue
		Serial asn1.RawValue
	}
	if _, err := asn1.Unmarshal(si.SID.FullBytes, &sid); err != nil || !bytes.Equal(sid.Issuer.FullBytes, cert.RawIssuer) {
		t.Errorf("signer identifier does not name the embedded certificate's issuer: %v", err)
	}
	return cert
}

// xrefOffsets walks the xref chain of a PDF and returns the offset of every
// object, checking each offset points at "N G obj".
func xrefOffsets(t *testing.T, pdf []byte) map[int]int {
	t.Helper()
	out := map[int]int{}
	startRE := regexp.MustCompile(`startxref\s+(\d+)\s+%%EOF\s*$`)
	m := startRE.FindSubmatch(pdf)
	if m == nil {
		t.Fatal("no startxref")
	}
	pos, _ := strconv.Atoi(string(m[1]))
	for pos > 0 {
		tail := string(pdf[pos:])
		if !strings.HasPrefix(tail, "xref") {
			t.Fatalf("startxref %d does not point at xref: %q", pos, tail[:min(20, len(tail))])
		}
		lines := strings.Split(tail, "\n")
		i := 1
		for ; i < len(lines) && !strings.HasPrefix(lines[i], "trailer"); i++ {
			var start, n int
			if _, err := fmt.Sscanf(lines[i], "%d %d", &start, &n); err != nil {
				t.Fatalf("subsection %q: %v", lines[i], err)
			}
			for k := 0; k < n; k++ {
				i++
				e := strings.Fields(lines[i])
				off, _ := strconv.Atoi(e[0])
				if e[2] == "n" {
					if _, seen := out[start+k]; !seen {
						out[start+k] = off
					}
				}
			}
		}
		trailer := strings.Join(lines[i:], "\n")
		prev := regexp.MustCompile(`/Prev (\d+)`).FindStringSubmatch(trailer[:strings.Index(trailer, "startxref")])
		if prev == nil {
			break
		}
		pos, _ = strconv.Atoi(prev[1])
	}
	for num, off := range out {
		want := fmt.Sprintf("%d 0 obj", num)
		if !bytes.HasPrefix(pdf[off:], []byte(want)) {
			t.Errorf("object %d: offset %d points at %q", num, off, pdf[off:min(off+20, len(pdf))])
		}
	}
	return out
}

func TestSignAddsAVerifiableDetachedSignatureAsAnIncrementalUpdate(t *testing.T) {
	for _, name := range []string{"rsa.p12", "ec.p12"} {
		for _, pages := range []int{1, 3} {
			s := newSigner(t, name)
			orig := samplePDF(t, pages)
			out, err := s.Sign(orig)
			if err != nil {
				t.Fatalf("%s/%d pages: %v", name, pages, err)
			}
			if !bytes.HasPrefix(out, orig) {
				t.Fatalf("%s: the original bytes were changed (a signed PDF must only be appended to)", name)
			}
			signed, blob := signature(t, out)
			cert := verifyCMS(t, signed, blob)
			if !strings.Contains(cert.Subject.String(), "Test") {
				t.Errorf("signer %v", cert.Subject)
			}
			s1 := string(out[len(orig):])
			for _, want := range []string{"/Type /Sig", "/Filter /Adobe.PPKLite", "/SubFilter /ETSI.CAdES.detached", "/FT /Sig", "/AcroForm", "/SigFlags 3", "/Annots", "/M (D:20261103050000Z)"} {
				if !strings.Contains(s1, want) {
					t.Errorf("%s: update lacks %s", name, want)
				}
			}
			offs := xrefOffsets(t, out)
			if len(offs) < 20 {
				t.Errorf("%s: only %d objects reachable through the xref chain", name, len(offs))
			}
			// The document still reads: the old objects are where they were.
			if !bytes.Contains(out, []byte("/Prev ")) {
				t.Error("the new xref section does not chain to the old one")
			}
		}
	}
}

// OpenSSL is an independent implementation of CMS: when it is installed, it
// must accept the signature, and refuse it over changed bytes.
func TestOpenSSLVerifiesTheSignature(t *testing.T) {
	bin, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not installed")
	}
	s := newSigner(t, "rsa.p12")
	out, err := s.Sign(samplePDF(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	signed, blob := signature(t, out)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sig, content := write("sig.der", blob), write("signed.bin", signed)
	verify := func(content string) ([]byte, error) {
		return exec.Command(bin, "cms", "-verify", "-inform", "DER", "-in", sig, "-content", content, "-binary", "-noverify", "-out", os.DevNull).CombinedOutput()
	}
	if o, err := verify(content); err != nil {
		t.Fatalf("openssl rejected the signature: %v\n%s", err, o)
	}
	bad := append([]byte{}, signed...)
	bad[len(bad)/2] ^= 1
	if _, err := verify(write("bad.bin", bad)); err == nil {
		t.Error("openssl accepted the signature over changed bytes")
	}
}

func TestNewReadsPKCS12AndRefusesWhatItCannotUse(t *testing.T) {
	now := func() time.Time { return signedAt }
	if _, err := pades.New(fixture(t, "rsa.p12"), "wrong", now); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := pades.New([]byte("not a pkcs12"), password, now); err == nil {
		t.Error("garbage accepted")
	}
	_, err := pades.New(fixture(t, "modern-aes.p12"), password, now)
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Errorf("a PKCS#12 encrypted with AES must say how to export one that works: %v", err)
	}
	// A certificate outside its validity is refused when loaded, not at the first invoice.
	if _, err := pades.New(fixture(t, "rsa.p12"), password, func() time.Time { return signedAt.AddDate(20, 0, 0) }); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired certificate: %v", err)
	}
	if _, err := pades.New(fixture(t, "rsa.p12"), password, func() time.Time { return signedAt.AddDate(-20, 0, 0) }); err == nil || !strings.Contains(err.Error(), "not yet valid") {
		t.Errorf("not yet valid certificate: %v", err)
	}
	s := newSigner(t, "rsa.p12")
	info := s.Info()
	if !strings.Contains(info.Subject, "Test Signatory") || info.NotAfter.Before(signedAt) || len(info.Fingerprint) != 64 {
		t.Errorf("info = %+v", info)
	}
}

func TestSignRefusesWhatItCannotSignSafely(t *testing.T) {
	s := newSigner(t, "rsa.p12")
	if _, err := s.Sign([]byte("%PDF-1.4 but not really")); err == nil {
		t.Error("garbage was signed")
	}
	once, err := s.Sign(samplePDF(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sign(once); err == nil {
		t.Error("a PDF that already has an AcroForm was signed again")
	}
}
