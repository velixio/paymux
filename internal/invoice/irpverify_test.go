package invoice_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// irpKey is a stand-in for the portal's signing key.
type irpKey struct {
	key *rsa.PrivateKey
	pem string
	x5t string
}

func newIRPKey(t *testing.T) irpKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "IRP test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(der)
	return irpKey{key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), x5t: base64.RawURLEncoding.EncodeToString(sum[:])}
}

// qr is a signed QR token for the invoice, signed by k under header.
func (k irpKey) qr(t *testing.T, header map[string]any, irn, docNo, gstin string) string {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"SellerGstin": gstin, "DocNo": docNo, "Irn": irn})
	payload, _ := json.Marshal(map[string]any{"data": string(data), "iss": "NIC"})
	h, _ := json.Marshal(header)
	b := base64.RawURLEncoding.EncodeToString
	in := b(h) + "." + b(payload)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b(sig)
}

func (e *env) setIRPKeys(keys ...string) {
	e.t.Helper()
	raw, _ := json.Marshal(keys)
	e.setOrg("einvoice_irp_keys", string(raw))
}

// With the portal's key configured, a signed QR code is accepted only when the
// portal's signature on it is valid.
func TestRecordEInvoiceVerifiesThePortalsSignature(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		inv := only(t, e, p.ID)
		portal, impostor := newIRPKey(t), newIRPKey(t)
		hdr := map[string]any{"alg": "RS256", "typ": "JWT", "x5t": portal.x5t}
		record := func(qr string) error {
			_, err := e.inv.RecordEInvoice(ctx, audit.System, inv.ID, invoice.EInvoiceRecord{IRN: irnHex, AckNo: "112610012345678",
				AckDate: "2026-10-18 10:15:30", SignedQR: qr})
			return err
		}
		// Without a key the claims are checked and the signature is not (as before).
		if err := record(fakeQR(irnHex, inv.Number, supplierGSTIN)); err != nil {
			t.Fatalf("no key configured: %v", err)
		}
		e.setIRPKeys(portal.pem)
		if err := record(portal.qr(t, hdr, irnHex, inv.Number, supplierGSTIN)); err != nil {
			t.Fatalf("a genuine token: %v", err)
		}
		for name, qr := range map[string]string{
			"the old unsigned test token":  fakeQR(irnHex, inv.Number, supplierGSTIN),
			"signed by someone else":       impostor.qr(t, map[string]any{"alg": "RS256", "x5t": portal.x5t}, irnHex, inv.Number, supplierGSTIN),
			"alg none":                     base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","x5t":"`+portal.x5t+`"}`)) + "." + strings.Split(portal.qr(t, hdr, irnHex, inv.Number, supplierGSTIN), ".")[1] + ".",
			"names no key":                 portal.qr(t, map[string]any{"alg": "RS256"}, irnHex, inv.Number, supplierGSTIN),
			"names a key that is not ours": portal.qr(t, map[string]any{"alg": "RS256", "x5t": impostor.x5t}, irnHex, inv.Number, supplierGSTIN),
		} {
			err := record(qr)
			// (An alg-none token has an empty signature and is refused as malformed first.)
			if !errors.Is(err, invoice.ErrInvalid) || (name != "alg none" && !strings.Contains(strings.ToLower(err.Error()), "signature")) {
				t.Errorf("%s: err = %v, want ErrInvalid about the signature", name, err)
			}
		}
		// A genuine signature on a token for another document still fails the claims check.
		if err := record(portal.qr(t, hdr, irnHex, "INV/9999/000009", supplierGSTIN)); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("another document: %v", err)
		}
	})
}

func TestIRPKeysSettingAcceptsOnlyUsableKeys(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		k := newIRPKey(t)
		raw, _ := json.Marshal([]string{k.pem})
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "einvoice_irp_keys", json.RawMessage(raw)); err != nil {
			t.Fatalf("a certificate: %v", err)
		}
		for name, v := range map[string]string{"not pem": `["hello"]`, "not a list": `"x"`, "a number": `[1]`, "empty entry": `[""]`} {
			if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "einvoice_irp_keys", json.RawMessage(v)); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		if err := e.set.SetOrg(ctx, audit.System, store.DefaultOrgID, "einvoice_irp_keys", json.RawMessage(`[]`)); err != nil {
			t.Errorf("clearing the keys: %v", err)
		}
	})
}
