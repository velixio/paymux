package invoice_test

import (
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Whether the provider sells as merchant of record is a fact of the account
// (stored when it was created), not only of the connector's registry entry.
func TestNoInvoiceForAnAccountThatIsMerchantOfRecord(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		p, _ := e.pay(118000, "INR", intraB2C, "pa_dynmor")
		if n := len(e.invoices(p.ID)); n != 0 {
			t.Errorf("a merchant-of-record account got %d invoices", n)
		}
		p2, _ := e.pay(118000, "INR", intraB2C, "pa_1")
		if n := len(e.invoices(p2.ID)); n != 1 {
			t.Errorf("an ordinary account got %d invoices", n)
		}
	})
}
