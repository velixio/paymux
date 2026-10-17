package invoice_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/invoice/einvoice"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

func b2bCustomer(t *testing.T, prefix14, state, line1, city, pin string) *core.CustomerInput {
	return &core.CustomerInput{Name: "Accounts", Email: "accounts@example.com", Billing: store.Billing{
		GSTIN: validGSTIN(t, prefix14), LegalName: "Acme Traders LLP", StateCode: state, Country: "IN", Line1: line1, City: city, PostalCode: pin}}
}

func doc(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return m
}

func field(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, _ := cur.(map[string]any)
		cur = mm[p]
	}
	return cur
}

func TestEInvoiceJSONForIntraInterAndExportInvoices(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setFX("USD", "2026-10-17", "83.2500")
		pIntra, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		pInter, _ := e.pay(118000, "INR", b2bCustomer(t, "29ABCDE1234F1Z", "29", "5 Residency Road", "Bengaluru", "560025"), "pa_1")
		pExport, _ := e.pay(5000, "USD", usBuyer, "pa_1")

		intra := only(t, e, pIntra.ID)
		res, err := e.inv.EInvoice(ctx, intra.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := einvoice.Validate(res.JSON); err != nil {
			t.Fatalf("intra-state document does not validate: %v\n%s", err, res.JSON)
		}
		d := doc(t, res.JSON)
		for path, want := range map[string]any{
			"TranDtls.SupTyp": "B2B", "DocDtls.No": intra.Number, "DocDtls.Dt": "17/10/2026", "SellerDtls.Gstin": supplierGSTIN, "SellerDtls.Pin": float64(400001),
			"SellerDtls.Loc": "Mumbai", "SellerDtls.Addr1": "1 MG Road", "BuyerDtls.Pin": float64(400050), "BuyerDtls.Loc": "Mumbai", "BuyerDtls.Addr1": "22 Linking Road",
			"BuyerDtls.Pos": "27", "BuyerDtls.LglNm": "Acme Traders LLP", "ValDtls.AssVal": float64(1000), "ValDtls.CgstVal": float64(90), "ValDtls.SgstVal": float64(90),
			"ValDtls.IgstVal": float64(0), "ValDtls.TotInvVal": float64(1180),
		} {
			if got := field(d, strings.Split(path, ".")...); got != want {
				t.Errorf("intra %s = %v, want %v", path, got, want)
			}
		}
		if item := d["ItemList"].([]any)[0].(map[string]any); item["HsnCd"] != "998315" || item["PrdDesc"] != "Voicestudio Pro - Oct 2026" || item["GstRt"] != float64(18) {
			t.Errorf("item %v", item)
		}

		inter, err := e.inv.EInvoice(ctx, only(t, e, pInter.ID).ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := einvoice.Validate(inter.JSON); err != nil {
			t.Fatalf("inter-state document does not validate: %v", err)
		}
		di := doc(t, inter.JSON)
		if field(di, "BuyerDtls", "Pos") != "29" || field(di, "ValDtls", "IgstVal") != float64(180) || field(di, "ValDtls", "CgstVal") != float64(0) {
			t.Errorf("inter-state %v", di["ValDtls"])
		}

		export, err := e.inv.EInvoice(ctx, only(t, e, pExport.ID).ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := einvoice.Validate(export.JSON); err != nil {
			t.Fatalf("export document does not validate: %v\n%s", err, export.JSON)
		}
		dx := doc(t, export.JSON)
		for path, want := range map[string]any{
			"TranDtls.SupTyp": "EXPWOP", "BuyerDtls.Gstin": "URP", "BuyerDtls.Pos": "96", "ExpDtls.ForCur": "USD", "ExpDtls.CntCode": "US",
			"ValDtls.TotInvValFc": float64(50), "ValDtls.TotInvVal": float64(4162.5), "ValDtls.IgstVal": float64(0),
		} {
			if got := field(dx, strings.Split(path, ".")...); got != want {
				t.Errorf("export %s = %v, want %v", path, got, want)
			}
		}
	})
}

func TestEInvoiceRefusesWhatCannotBeBuilt(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(ent *store.TaxEntity, _ *store.AppTaxProfile) { ent.Address = "Mumbai" }) // no PIN code
		b2c, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		if _, err := e.inv.EInvoice(ctx, only(t, e, b2c.ID).ID); !errors.Is(err, invoice.ErrInvalid) || !strings.Contains(err.Error(), "B2B and export") {
			t.Errorf("a B2C invoice: %v", err)
		}
		b2b, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		_, err := e.inv.EInvoice(ctx, only(t, e, b2b.ID).ID)
		var pe *einvoice.Problems
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), "PIN") || !strings.Contains(err.Error(), "seller") {
			t.Errorf("a seller address without a PIN should be explained: %v", err)
		}
		export, _ := e.pay(5000, "USD", usBuyer, "pa_1") // no FX rate yet
		if _, err := e.inv.EInvoice(ctx, only(t, e, export.ID).ID); err == nil || !strings.Contains(err.Error(), "FX rate") {
			t.Errorf("an export waiting for its FX rate: %v", err)
		}
		if _, err := e.inv.EInvoice(ctx, "inv_nosuch"); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("unknown invoice: %v", err)
		}
	})
}

func fakeQR(irn, docNo, gstin string) string {
	data, _ := json.Marshal(map[string]any{"SellerGstin": gstin, "BuyerGstin": "29AAGCB7383J1Z4", "DocNo": docNo, "DocTyp": "INV", "DocDt": "17/10/2026", "TotInvVal": 1180, "ItemCnt": 1, "MainHsnCode": "998315", "Irn": irn})
	payload, _ := json.Marshal(map[string]any{"data": string(data), "iss": "NIC"})
	b := func(s []byte) string { return base64.RawURLEncoding.EncodeToString(s) }
	return b([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b(payload) + "." + b([]byte("signature-bytes"))
}

var irnHex = strings.Repeat("0123456789abcdef", 4)

func TestRecordEInvoiceStoresChecksAndRendersAgain(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		inv := only(t, e, p.ID)
		e.runJobs() // the PDF exists now
		before := only(t, e, p.ID)
		if before.PDFBlobID == "" {
			t.Fatal("no pdf yet")
		}
		in := invoice.EInvoiceRecord{IRN: strings.ToUpper(irnHex), AckNo: "112610012345678", AckDate: "2026-10-18 10:15:30",
			SignedQR: "  " + fakeQR(irnHex, inv.Number, supplierGSTIN) + "\n"}
		got, err := e.inv.RecordEInvoice(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_op"}, inv.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		wantAck := time.Date(2026, 10, 18, 10, 15, 30, 0, time.FixedZone("IST", 19800))
		if got.IRN != irnHex || got.AckNo != "112610012345678" || !got.AckDate.Equal(wantAck) || got.SignedQR != strings.TrimSpace(in.SignedQR) || got.EInvoiceAt.IsZero() {
			t.Fatalf("recorded %+v", got)
		}
		var aud []store.AuditRow
		_ = db.Read(ctx, func(q store.Querier) (err error) {
			aud, err = store.ListAudit(ctx, q, store.DefaultOrgID, "invoice.einvoice_record", 10)
			return
		})
		if len(aud) != 1 || aud[0].ActorID != "usr_op" || aud[0].Target != inv.ID {
			t.Errorf("audit %+v", aud)
		}
		if blob := string(aud[0].After); strings.Contains(blob, "signature-bytes") || strings.Contains(blob, in.SignedQR[:20]) {
			t.Errorf("the audit entry should not carry the QR payload: %s", blob)
		}
		// The PDF is made again, with the QR code on it.
		if mid := only(t, e, p.ID); mid.PDFBlobID != "" {
			t.Errorf("the stale PDF should be dropped, still %q", mid.PDFBlobID)
		}
		e.runJobs()
		after := only(t, e, p.ID)
		if after.PDFBlobID == "" || after.PDFBlobID == before.PDFBlobID {
			t.Errorf("PDF blob %q -> %q: want a new one", before.PDFBlobID, after.PDFBlobID)
		}
		if e.mailer.count() != 0 {
			t.Errorf("re-rendering must not send the invoice again (%d mails)", e.mailer.count())
		}
		// A correction replaces the data.
		in.AckNo = "112610099999999"
		if again, err := e.inv.RecordEInvoice(ctx, audit.System, inv.ID, in); err != nil || again.AckNo != "112610099999999" {
			t.Errorf("correction: %v %+v", err, again)
		}
	})
}

func TestRecordEInvoiceRejectsWhatDoesNotBelong(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p1, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		p2, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		b2c, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		i1, i2, ic := only(t, e, p1.ID), only(t, e, p2.ID), only(t, e, b2c.ID)
		good := func() invoice.EInvoiceRecord {
			return invoice.EInvoiceRecord{IRN: irnHex, AckNo: "112610012345678", AckDate: "2026-10-18 10:15:30", SignedQR: fakeQR(irnHex, i1.Number, supplierGSTIN)}
		}
		cases := map[string]func(*invoice.EInvoiceRecord){
			"IRN too short":           func(r *invoice.EInvoiceRecord) { r.IRN = irnHex[:40] },
			"IRN not hex":             func(r *invoice.EInvoiceRecord) { r.IRN = strings.Repeat("xyz1", 16) },
			"ack number letters":      func(r *invoice.EInvoiceRecord) { r.AckNo = "ABC" },
			"ack number too long":     func(r *invoice.EInvoiceRecord) { r.AckNo = strings.Repeat("1", 20) },
			"ack date unreadable":     func(r *invoice.EInvoiceRecord) { r.AckDate = "yesterday" },
			"ack date far future":     func(r *invoice.EInvoiceRecord) { r.AckDate = "2031-01-01 00:00:00" },
			"ack date before invoice": func(r *invoice.EInvoiceRecord) { r.AckDate = "2026-01-01 00:00:00" },
			"QR is not a token":       func(r *invoice.EInvoiceRecord) { r.SignedQR = "just text" },
			"QR payload unreadable":   func(r *invoice.EInvoiceRecord) { r.SignedQR = "a.b.c" },
			"QR is for another IRN": func(r *invoice.EInvoiceRecord) {
				r.SignedQR = fakeQR(strings.Repeat("f", 64), i1.Number, supplierGSTIN)
			},
			"QR is for another doc":    func(r *invoice.EInvoiceRecord) { r.SignedQR = fakeQR(irnHex, i2.Number, supplierGSTIN) },
			"QR is for another seller": func(r *invoice.EInvoiceRecord) { r.SignedQR = fakeQR(irnHex, i1.Number, "29AAGCB7383J1Z4") },
			"QR too large": func(r *invoice.EInvoiceRecord) {
				r.SignedQR = fakeQR(irnHex, i1.Number, supplierGSTIN) + strings.Repeat("A", 5000)
			},
			"empty": func(r *invoice.EInvoiceRecord) { *r = invoice.EInvoiceRecord{} },
		}
		for name, mut := range cases {
			in := good()
			mut(&in)
			if _, err := e.inv.RecordEInvoice(ctx, audit.System, i1.ID, in); !errors.Is(err, invoice.ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			}
		}
		if _, err := e.inv.RecordEInvoice(ctx, audit.System, ic.ID, good()); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("a B2C invoice has no e-invoice: %v", err)
		}
		if _, err := e.inv.RecordEInvoice(ctx, audit.System, "inv_nosuch", good()); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("unknown invoice: %v", err)
		}
		// One IRN belongs to one invoice.
		if _, err := e.inv.RecordEInvoice(ctx, audit.System, i1.ID, good()); err != nil {
			t.Fatal(err)
		}
		dup := good()
		dup.SignedQR = fakeQR(irnHex, i2.Number, supplierGSTIN)
		if _, err := e.inv.RecordEInvoice(ctx, audit.System, i2.ID, dup); !errors.Is(err, invoice.ErrInvalid) || !strings.Contains(err.Error(), "another invoice") {
			t.Errorf("the same IRN on a second invoice: %v", err)
		}
		if got := only(t, e, p2.ID); got.IRN != "" {
			t.Error("the refused IRN was stored")
		}
	})
}

func TestEInvoiceMonthExport(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.setFX("USD", "2026-10-17", "83.2500")
		pa, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", "400050"), "pa_1")
		pb, _ := e.pay(236000, "INR", b2bCustomer(t, "29ABCDE1234F1Z", "29", "5 Residency Road", "Bengaluru", "560025"), "pa_1")
		e.pay(118000, "INR", intraB2C, "pa_1")
		pe, _ := e.pay(5000, "USD", usBuyer, "pa_1")
		noPin, _ := e.pay(118000, "INR", b2bCustomer(t, "27ABCDE1234F1Z", "27", "22 Linking Road", "Mumbai", ""), "pa_1")
		ia, ib, ie, ip := only(t, e, pa.ID), only(t, e, pb.ID), only(t, e, pe.ID), only(t, e, noPin.ID)

		m, err := e.inv.EInvoiceMonth(ctx, "te_1", "2026-10", false)
		if err != nil {
			t.Fatal(err)
		}
		status := map[string]string{}
		for _, r := range m.Rows {
			status[r.Invoice.ID] = r.Status
		}
		if status[ia.ID] != "ready" || status[ib.ID] != "ready" || status[ie.ID] != "ready" || status[ip.ID] != "problem" || len(m.Rows) != 4 {
			t.Fatalf("rows %v", status)
		}
		if m.SkippedB2C != 1 || m.Ready != 3 || m.Problems != 1 {
			t.Errorf("counts: ready=%d problems=%d b2c=%d", m.Ready, m.Problems, m.SkippedB2C)
		}
		for _, r := range m.Rows {
			if r.Invoice.ID == ip.ID && (len(r.Problems) == 0 || !strings.Contains(strings.Join(r.Problems, " "), "PIN")) {
				t.Errorf("the problem should be explained: %v", r.Problems)
			}
		}
		if err := einvoice.ValidateBatch(m.JSON); err != nil {
			t.Fatalf("the batch does not validate: %v", err)
		}
		var docs []map[string]any
		_ = json.Unmarshal(m.JSON, &docs)
		if len(docs) != 3 {
			t.Fatalf("batch has %d documents, want the 3 that are ready", len(docs))
		}
		if m.Filename != "einvoice-te_1-2026-10.json" {
			t.Errorf("filename %q", m.Filename)
		}

		// After the operator records the portal's answer the invoice is done and leaves the default export.
		if _, err := e.inv.RecordEInvoice(ctx, audit.System, ia.ID, invoice.EInvoiceRecord{IRN: irnHex, AckNo: "1", AckDate: "2026-10-18 10:00:00",
			SignedQR: fakeQR(irnHex, ia.Number, supplierGSTIN)}); err != nil {
			t.Fatal(err)
		}
		m2, _ := e.inv.EInvoiceMonth(ctx, "te_1", "2026-10", false)
		_ = json.Unmarshal(m2.JSON, &docs)
		st := map[string]string{}
		for _, r := range m2.Rows {
			st[r.Invoice.ID] = r.Status
		}
		if st[ia.ID] != "done" || len(docs) != 2 || m2.Done != 1 {
			t.Errorf("after recording: %v, %d documents", st, len(docs))
		}
		m3, _ := e.inv.EInvoiceMonth(ctx, "te_1", "2026-10", true)
		_ = json.Unmarshal(m3.JSON, &docs)
		if len(docs) != 3 {
			t.Errorf("including the done ones: %d documents", len(docs))
		}

		if _, err := e.inv.EInvoiceMonth(ctx, "te_1", "October", false); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("bad month: %v", err)
		}
		if _, err := e.inv.EInvoiceMonth(ctx, "te_nosuch", "2026-10", false); !errors.Is(err, invoice.ErrInvalid) {
			t.Errorf("unknown tax entity: %v", err)
		}
		if empty, err := e.inv.EInvoiceMonth(ctx, "te_1", "2026-09", false); err != nil || len(empty.Rows) != 0 || strings.TrimSpace(string(empty.JSON)) != "[]" {
			t.Errorf("an empty month: %v %+v", err, empty)
		}
	})
}
