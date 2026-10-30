package invoice_test

import (
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/internal/tax"
)

// The org setting einvoice_igst_refund_claim decides RefClm of an export that
// paid IGST (auto: claim). An export under a LUT never claims.
func TestEInvoiceRefundClaimIsAnOrgSetting(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, func(en *store.TaxEntity, _ *store.AppTaxProfile) {
			en.LUTValidTo = time.Date(2026, 10, 9, 0, 0, 0, 0, tax.IST) // expired on the 17th: exports pay IGST
		})
		e.setFX("USD", "2026-10-17", "83.2500")
		p, _ := e.pay(5000, "USD", usBuyer, "pa_1")
		inv := only(t, e, p.ID)
		if inv.SupplyType != "export_igst" {
			t.Fatalf("setup: %s", inv.SupplyType)
		}
		refClm := func() string {
			res, err := e.inv.EInvoice(ctx, inv.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range []string{"Y", "N"} {
				if strings.Contains(string(res.JSON), `"RefClm": "`+v+`"`) {
					return v
				}
			}
			t.Fatalf("no RefClm in %s", res.JSON)
			return ""
		}
		if got := refClm(); got != "Y" {
			t.Errorf("default (auto): RefClm = %s, want Y", got)
		}
		e.setOrg("einvoice_igst_refund_claim", `"N"`)
		if got := refClm(); got != "N" {
			t.Errorf("forced N: RefClm = %s", got)
		}
		e.setOrg("einvoice_igst_refund_claim", `"Y"`)
		if got := refClm(); got != "Y" {
			t.Errorf("forced Y: RefClm = %s", got)
		}
	})
}
