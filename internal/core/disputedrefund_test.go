package core_test

import (
	"errors"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/store"
)

// A payment in dispute, or one whose dispute was lost, has had its money taken
// back by the card network. Refunding it as well pays the customer twice.
func TestAPaymentInDisputeCannotBeRefundedByUs(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		for _, flag := range []string{"disputed", "dispute_lost"} {
			p, _ := paidPayment(t, e, 1000)
			err := e.db.Write(ctx, func(tx store.Tx) error {
				p.Flags = append(p.Flags, flag)
				return store.UpdatePayment(ctx, tx, &p, time.Now())
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, by := range []string{"api", "dashboard"} {
				_, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: by, PaymentID: p.ID, Amount: 100})
				if !errors.Is(err, core.ErrNotRefundable) {
					t.Errorf("%s by %s: %v", flag, by, err)
				}
			}
		}
	})
}
