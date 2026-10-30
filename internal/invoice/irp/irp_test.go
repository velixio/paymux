package irp_test

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/invoice/irp"
)

// The IRP signs the SignedQRCode (and SignedInvoice) as a JWT, RS256, with its
// own key. Verification is strict: only RS256, never "none" or an HMAC under
// the public key, only a key Paymux was given, and the token must name that
// key (kid or x5t) and name it correctly.

type signer struct {
	key  *rsa.PrivateKey
	cert *x509.Certificate
	pem  string
	x5t  string
}

func newSigner(t *testing.T, bits int) signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "IRP test"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), SubjectKeyId: []byte{1, 2, 3, 4, 5, 6, 7, 8}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	sum := sha1.Sum(der)
	return signer{key: key, cert: cert, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), x5t: base64.RawURLEncoding.EncodeToString(sum[:])}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// token signs payload under header (a map, so tests can bend it).
func (s signer) token(t *testing.T, header map[string]any, payload string) string {
	t.Helper()
	h, _ := json.Marshal(header)
	in := b64(h) + "." + b64([]byte(payload))
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func (s signer) good(t *testing.T) string {
	return s.token(t, map[string]any{"alg": "RS256", "typ": "JWT", "x5t": s.x5t}, `{"data":"{\"Irn\":\"abc\"}","iss":"NIC"}`)
}

func keys(t *testing.T, pems ...string) []irp.Key {
	t.Helper()
	k, err := irp.ParseKeys(pems)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestVerifyAcceptsATokenSignedByAConfiguredKey(t *testing.T) {
	a, b := newSigner(t, 2048), newSigner(t, 2048)
	ks := keys(t, a.pem, b.pem)
	payload, err := irp.Verify(a.good(t), ks)
	if err != nil || !strings.Contains(string(payload), "NIC") {
		t.Fatalf("verify: %s %v", payload, err)
	}
	// The second key, picked by its own thumbprint.
	if _, err := irp.Verify(b.good(t), ks); err != nil {
		t.Errorf("second key: %v", err)
	}
	// kid is the hex of the certificate's subject key id.
	if _, err := irp.Verify(a.token(t, map[string]any{"alg": "RS256", "kid": "0102030405060708"}, `{"data":"x"}`), ks); err != nil {
		t.Errorf("kid: %v", err)
	}
}

func TestVerifyRefusesWhatIsNotStrictlyAnIRPSignature(t *testing.T) {
	a, other := newSigner(t, 2048), newSigner(t, 2048)
	ks := keys(t, a.pem)
	good := a.good(t)
	parts := strings.Split(good, ".")
	hdr := func(m map[string]any) string { h, _ := json.Marshal(m); return b64(h) }
	hs256 := func() string { // the classic attack: an HMAC keyed with the public key's bytes
		in := hdr(map[string]any{"alg": "HS256", "x5t": a.x5t}) + "." + parts[1]
		m := hmac.New(sha256.New, a.cert.Raw)
		m.Write([]byte(in))
		return in + "." + b64(m.Sum(nil))
	}()
	cases := map[string]string{
		"alg none, no signature":   hdr(map[string]any{"alg": "none", "x5t": a.x5t}) + "." + parts[1] + ".",
		"alg none, with signature": hdr(map[string]any{"alg": "none", "x5t": a.x5t}) + "." + parts[1] + "." + parts[2],
		"alg HS256 under the key":  hs256,
		"alg rs256 lower case":     a.token(t, map[string]any{"alg": "rs256", "x5t": a.x5t}, `{"data":"x"}`),
		"alg RS384":                a.token(t, map[string]any{"alg": "RS384", "x5t": a.x5t}, `{"data":"x"}`),
		"no alg":                   a.token(t, map[string]any{"x5t": a.x5t}, `{"data":"x"}`),
		"alg not a string":         a.token(t, map[string]any{"alg": []string{"RS256"}, "x5t": a.x5t}, `{"data":"x"}`),
		"no kid or x5t":            a.token(t, map[string]any{"alg": "RS256"}, `{"data":"x"}`),
		"wrong x5t":                a.token(t, map[string]any{"alg": "RS256", "x5t": other.x5t}, `{"data":"x"}`),
		"wrong kid":                a.token(t, map[string]any{"alg": "RS256", "kid": "deadbeef"}, `{"data":"x"}`),
		"right x5t, wrong kid":     a.token(t, map[string]any{"alg": "RS256", "x5t": a.x5t, "kid": "deadbeef"}, `{"data":"x"}`),
		"signed by another key":    other.token(t, map[string]any{"alg": "RS256", "x5t": a.x5t}, `{"data":"x"}`),
		"tampered payload":         parts[0] + "." + b64([]byte(`{"data":"forged"}`)) + "." + parts[2],
		"tampered header":          hdr(map[string]any{"alg": "RS256", "x5t": a.x5t, "extra": 1}) + "." + parts[1] + "." + parts[2],
		"empty signature":          parts[0] + "." + parts[1] + ".",
		"truncated signature":      good[:len(good)-10],
		"two parts":                parts[0] + "." + parts[1],
		"four parts":               good + ".x",
		"padded base64":            parts[0] + "=." + parts[1] + "." + parts[2],
		"not base64":               "!!!." + parts[1] + "." + parts[2],
		"embedded key":             a.token(t, map[string]any{"alg": "RS256", "x5t": a.x5t, "jwk": map[string]any{"kty": "RSA"}}, `{"data":"x"}`),
		"key url":                  a.token(t, map[string]any{"alg": "RS256", "x5t": a.x5t, "jku": "https://evil.example.com/keys"}, `{"data":"x"}`),
		"cert url":                 a.token(t, map[string]any{"alg": "RS256", "x5t": a.x5t, "x5u": "https://evil.example.com/c.pem"}, `{"data":"x"}`),
		"critical extension":       a.token(t, map[string]any{"alg": "RS256", "x5t": a.x5t, "crit": []string{"exp"}, "exp": 1}, `{"data":"x"}`),
		"empty":                    "",
	}
	for name, tok := range cases {
		if _, err := irp.Verify(tok, ks); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// With no key configured nothing can be verified.
	if _, err := irp.Verify(good, nil); err == nil {
		t.Error("a token was verified with no keys")
	}
}

func TestParseKeysRefusesWhatIsNotAnUsableKey(t *testing.T) {
	small := newSigner(t, 1024)
	for name, in := range map[string][]string{
		"too small":   {small.pem},
		"not pem":     {"hello"},
		"empty":       {""},
		"wrong block": {string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}))},
	} {
		if _, err := irp.ParseKeys(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A bare public key works too, identified by kid only.
	a := newSigner(t, 2048)
	der, _ := x509.MarshalPKIXPublicKey(&a.key.PublicKey)
	bare := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	ks, err := irp.ParseKeys([]string{bare})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(der)
	kid := strings.ToLower(hexOf(sum[:]))
	if _, err := irp.Verify(a.token(t, map[string]any{"alg": "RS256", "kid": kid}, `{"data":"x"}`), ks); err != nil {
		t.Errorf("bare key by kid: %v", err)
	}
	if _, err := irp.Verify(a.good(t), ks); err == nil {
		t.Error("a bare key was matched by a thumbprint it does not have")
	}
}

func hexOf(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, h[c>>4], h[c&15])
	}
	return string(out)
}
