package invoice_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/invoice"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
)

const dscPassword = "test-dsc-pass"

func p12(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("pades", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func isSigned(pdf []byte) bool {
	return bytes.Contains(pdf, []byte("/ByteRange")) && bytes.Contains(pdf, []byte("ETSI.CAdES.detached"))
}

var byteRangeRE = regexp.MustCompile(`/ByteRange \[0 (\d+) (\d+) (\d+)\]`)

// opensslVerifies checks the signature of a signed PDF with OpenSSL, an
// implementation independent of the one that made it. It skips when OpenSSL is
// not installed.
func opensslVerifies(t *testing.T, pdf []byte) {
	t.Helper()
	bin, err := exec.LookPath("openssl")
	if err != nil {
		return
	}
	m := byteRangeRE.FindSubmatch(pdf)
	if m == nil {
		t.Fatal("no byte range")
	}
	a, _ := strconv.Atoi(string(m[1]))
	b, _ := strconv.Atoi(string(m[2]))
	raw, err := hex.DecodeString(string(pdf[a+1 : b-1]))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sig, content := filepath.Join(dir, "sig.der"), filepath.Join(dir, "signed.bin")
	_ = os.WriteFile(sig, raw, 0o600)
	_ = os.WriteFile(content, append(append([]byte{}, pdf[:a]...), pdf[b:]...), 0o600)
	if o, err := exec.Command(bin, "cms", "-verify", "-inform", "DER", "-in", sig, "-content", content, "-binary", "-noverify", "-out", os.DevNull).CombinedOutput(); err != nil {
		t.Errorf("openssl rejected the signature of the rendered document: %v\n%s", err, o)
	}
}

// The test certificates are valid from the day they were made, so the
// clock for these tests is later than that.
func dscEnv(t *testing.T, db *store.DB) *env {
	e := newEnv(t, db)
	e.setNow(time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC))
	return e
}

func TestDocumentsAreSignedWhenTheTaxEntityHasADSC(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := dscEnv(t, db)
		// Without a DSC: the signatory note only.
		p0, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		if isSigned(pdfOf(t, e, only(t, e, p0.ID).PDFBlobID)) {
			t.Fatal("a PDF was signed with no DSC")
		}

		info, err := e.inv.SetTaxEntityDSC(ctx, audit.System, "te_1", p12(t, "rsa.p12"), dscPassword)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(info.Subject, "Test Signatory") || info.NotAfter.Before(e.clock()) || len(info.Fingerprint) != 64 {
			t.Errorf("info = %+v", info)
		}
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.refund(p, 30000)
		e.runJobs()
		invPDF, cnPDF := pdfOf(t, e, only(t, e, p.ID).PDFBlobID), pdfOf(t, e, e.creditNotes(p.ID)[0].PDFBlobID)
		if !isSigned(invPDF) {
			t.Error("the invoice PDF is not signed")
		}
		if !isSigned(cnPDF) {
			t.Error("the credit note PDF is not signed")
		}
		opensslVerifies(t, invPDF)
		opensslVerifies(t, cnPDF)
		// Documents rendered before stay as they were.
		if isSigned(pdfOf(t, e, only(t, e, p0.ID).PDFBlobID)) {
			t.Error("an old PDF changed")
		}

		// The page of the entity shows which certificate signs.
		var ent store.TaxEntity
		_ = db.Read(ctx, func(q store.Querier) (err error) { ent, err = store.GetTaxEntity(ctx, q, "te_1"); return })
		if !strings.Contains(ent.DSCSubject, "Test Signatory") || ent.DSCNotAfter.IsZero() {
			t.Errorf("entity = %+v", ent)
		}

		// Removing it goes back to the note.
		if err := e.inv.ClearTaxEntityDSC(ctx, audit.System, "te_1"); err != nil {
			t.Fatal(err)
		}
		p2, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		if isSigned(pdfOf(t, e, only(t, e, p2.ID).PDFBlobID)) {
			t.Error("signed after the DSC was removed")
		}
	})
}

func TestTheDSCIsKeptSealedAndAudited(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := dscEnv(t, db)
		file := p12(t, "ec.p12")
		if _, err := e.inv.SetTaxEntityDSC(ctx, audit.System, "te_1", file, dscPassword); err != nil {
			t.Fatal(err)
		}
		var d store.EntityDSC
		var ok bool
		_ = db.Read(ctx, func(q store.Querier) (err error) { d, ok, err = store.GetTaxEntityDSC(ctx, q, "te_1"); return })
		if !ok || bytes.Contains(d.Enc, file) || bytes.Contains(d.Enc, []byte(dscPassword)) {
			t.Fatalf("stored in the clear or missing: ok=%v", ok)
		}
		// The audit log names the certificate but holds neither the file nor the password.
		var logged int
		_ = db.Read(ctx, func(q store.Querier) error {
			rows, err := q.QueryContext(ctx, "SELECT COALESCE(after,''), COALESCE(before,'') FROM audit_log WHERE action = 'tax_entity.dsc_set'")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var after, before string
				_ = rows.Scan(&after, &before)
				logged++
				if !strings.Contains(after, "Test EC Signatory") || strings.Contains(after+before, dscPassword) {
					t.Errorf("audit entry = %q", after)
				}
			}
			return nil
		})
		if logged != 1 {
			t.Errorf("audit entries = %d", logged)
		}
	})
}

func TestSettingADSCRefusesWhatCannotSign(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := dscEnv(t, db)
		for name, c := range map[string]struct {
			file []byte
			pass string
			want string
		}{
			"wrong password": {p12(t, "rsa.p12"), "nope", "password"},
			"not a pkcs12":   {[]byte("hello"), dscPassword, ""},
			"aes encryption": {p12(t, "modern-aes.p12"), dscPassword, "legacy"},
			"too large":      {bytes.Repeat([]byte{1}, 200<<10), dscPassword, "large"},
			"empty":          {nil, dscPassword, ""},
		} {
			_, err := e.inv.SetTaxEntityDSC(ctx, audit.System, "te_1", c.file, c.pass)
			if !errors.Is(err, invoice.ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := e.inv.SetTaxEntityDSC(ctx, audit.System, "te_nope", p12(t, "rsa.p12"), dscPassword); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("unknown entity: %v", err)
		}
		// Nothing was stored by any of them.
		_ = db.Read(ctx, func(q store.Querier) error {
			if _, ok, _ := store.GetTaxEntityDSC(ctx, q, "te_1"); ok {
				t.Error("a refused certificate was stored")
			}
			return nil
		})
	})
}

// A certificate that stops working must not stop invoices: they go out with
// the signatory note, and an operator is told.
func TestAnUnusableDSCDoesNotStopInvoices(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := dscEnv(t, db)
		if _, err := e.inv.SetTaxEntityDSC(ctx, audit.System, "te_1", p12(t, "rsa.p12"), dscPassword); err != nil {
			t.Fatal(err)
		}
		e.setNow(time.Date(2037, 1, 5, 5, 0, 0, 0, time.UTC)) // the certificate has expired
		p, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		e.runJobs()
		inv := only(t, e, p.ID)
		if inv.PDFBlobID == "" || isSigned(pdfOf(t, e, inv.PDFBlobID)) {
			t.Errorf("an invoice with an expired DSC: blob %q", inv.PDFBlobID)
		}
		found := false
		for _, k := range e.alerts.kinds() {
			found = found || k == "tax.dsc_unusable"
		}
		if !found {
			t.Errorf("no alert about the DSC: %v", e.alerts.kinds())
		}
	})
}
