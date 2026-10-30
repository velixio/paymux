package invoice_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/invoice"
	"github.com/velixio/paymux/internal/store"
)

func TestInvoiceViewCarriesTheRecordedEInvoice(t *testing.T) {
	plain, _ := json.Marshal(invoice.ViewInvoice(store.Invoice{ID: "inv_1", Number: "INV/1"}))
	for _, k := range []string{"irn", "ack_no", "ack_date"} {
		if strings.Contains(string(plain), `"`+k+`"`) {
			t.Errorf("an invoice without an e-invoice has no %s: %s", k, plain)
		}
	}
	ack := time.Date(2026, 10, 18, 4, 45, 30, 0, time.UTC)
	raw, _ := json.Marshal(invoice.ViewInvoice(store.Invoice{ID: "inv_1", Number: "INV/1", IRN: irnHex, AckNo: "112610012345678", AckDate: ack, SignedQR: "a.b.c"}))
	for _, want := range []string{`"irn":"` + irnHex + `"`, `"ack_no":"112610012345678"`, `"ack_date":"2026-10-18T04:45:30Z"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("view lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "a.b.c") {
		t.Error("the signed token is not part of the API view")
	}
}
