package checkout_test

import (
	"errors"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

// A refund fetched from the provider is checked against the refund it claims to
// be before it is applied, on the import path as on the poll path: the payment,
// the receipt, the amount and the currency have to agree.
func TestImportRefundChecksAKnownRefundBeforeApplyingIt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, st := paid(t, e)
		r, err := e.core.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 20000})
		if err != nil {
			t.Fatal(err)
		}
		for name, snap := range map[string]connector.RefundSnapshot{
			"another amount":   {ProviderRef: r.ProviderRef, ProviderRefundID: "rfnd_1", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 49999, Currency: "INR"},
			"another payment":  {ProviderRef: r.ProviderRef, ProviderRefundID: "rfnd_1", ProviderPaymentID: "pay_other", Status: connector.RefundSucceeded, Amount: 20000, Currency: "INR"},
			"another currency": {ProviderRef: r.ProviderRef, ProviderRefundID: "rfnd_1", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 20000, Currency: "USD"},
		} {
			_, err := checkout.ImportRefund(ctx, e.db, e.core, st.AccountID, snap)
			if !errors.Is(err, checkout.ErrNotOurs) {
				t.Errorf("%s: err = %v, want ErrNotOurs", name, err)
			}
			rows := refundRows(e, p.ID)
			if len(rows) != 1 || rows[0].Status == "succeeded" || e.get(p.ID).AmountRefunded != 0 {
				t.Errorf("%s: the refund was applied: %+v", name, rows)
			}
		}
		// The matching snapshot is applied.
		ok := connector.RefundSnapshot{ProviderRef: r.ProviderRef, ProviderRefundID: "rfnd_1", ProviderPaymentID: "pay_1", Status: connector.RefundSucceeded, Amount: 20000, Currency: "INR"}
		if imported, err := checkout.ImportRefund(ctx, e.db, e.core, st.AccountID, ok); err != nil || !imported {
			t.Fatalf("the right snapshot: %v %v", imported, err)
		}
		var got store.Refund
		_ = e.db.Read(ctx, func(q store.Querier) (err error) { got, err = store.GetRefund(ctx, q, r.ID); return })
		if got.Status != "succeeded" {
			t.Errorf("status %s", got.Status)
		}
	})
}
