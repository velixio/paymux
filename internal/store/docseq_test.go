package store_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// Documents issued in the same millisecond must list in numbering order, not
// in id order (ids carry random bits within a millisecond).
func TestDocumentListsFollowTheDocumentSequence(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		err := db.Write(ctx, func(tx store.Tx) error {
			// ids sort opposite to the sequence on purpose
			for _, d := range []struct {
				id  string
				seq int64
			}{{"inv_c", 1}, {"inv_b", 2}, {"inv_a", 3}} {
				inv := store.Invoice{ID: d.id, OrgID: "o", Mode: "test", AppID: "app_1", TaxEntityID: "te_1", PaymentID: "pay_1", AttemptID: "att_1",
					Number: fmt.Sprintf("INV/2627/%06d", d.seq), Seq: d.seq, SeriesPrefix: "INV", FiscalYear: "2627", IssuedAt: t0,
					SupplyType: "b2c", PlaceOfSupply: "27", Currency: "INR", Supplier: json.RawMessage(`{}`), Recipient: json.RawMessage(`{}`),
					Lines: json.RawMessage(`[]`), Taxable: 1, Total: 1, TemplateVersion: "v1", AccessTokenHash: "h" + d.id}
				if err := store.InsertInvoice(ctx, tx, inv); err != nil {
					return err
				}
				cn := store.CreditNote{ID: "crn_" + d.id[4:], OrgID: "o", Mode: "test", AppID: "app_1", TaxEntityID: "te_1", InvoiceID: "inv_c", PaymentID: "pay_1",
					RefundID: "ref_" + d.id[4:], Number: fmt.Sprintf("CN/2627/%06d", d.seq), Seq: d.seq, SeriesPrefix: "CN", FiscalYear: "2627",
					IssuedAt: t0, Currency: "INR", Lines: json.RawMessage(`[]`), Taxable: 1, Total: 1, TemplateVersion: "v1", AccessTokenHash: "h" + d.id}
				if err := store.InsertCreditNote(ctx, tx, cn); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.Read(ctx, func(q store.Querier) error {
			invs, err := store.InvoicesForPayment(ctx, q, "pay_1")
			if err != nil || len(invs) != 3 || invs[0].ID != "inv_c" || invs[1].ID != "inv_b" || invs[2].ID != "inv_a" || invs[0].Seq != 1 {
				t.Errorf("invoices %v %v", ids(invs, func(i store.Invoice) string { return i.ID }), err)
			}
			byInv, err := store.CreditNotesForInvoice(ctx, q, "inv_c")
			if err != nil || len(byInv) != 3 || byInv[0].ID != "crn_c" || byInv[2].ID != "crn_a" || byInv[2].Seq != 3 {
				t.Errorf("credit notes by invoice %v %v", ids(byInv, func(c store.CreditNote) string { return c.ID }), err)
			}
			byPay, err := store.CreditNotesForPayment(ctx, q, "pay_1")
			if err != nil || len(byPay) != 3 || byPay[0].ID != "crn_c" || byPay[1].ID != "crn_b" || byPay[2].ID != "crn_a" {
				t.Errorf("credit notes by payment %v %v", ids(byPay, func(c store.CreditNote) string { return c.ID }), err)
			}
			return nil
		})
	})
}

func ids[T any](l []T, f func(T) string) []string {
	var out []string
	for _, x := range l {
		out = append(out, f(x))
	}
	return out
}
