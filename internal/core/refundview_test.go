package core_test

import (
	"encoding/json"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/core/state"
)

func TestRefundViewShowsTheProviderRefundIDOnceKnown(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		p, _ := paidPayment(t, e, 1000)
		r, err := e.svc.CreateRefund(ctx, core.CreateRefundInput{Scope: e.scope, Actor: audit.System, InitiatedBy: "api", PaymentID: p.ID, Amount: 400})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(core.ViewRefund(r))
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		if _, present := v["provider_refund_id"]; present {
			t.Errorf("before the provider answered the field should be absent: %s", raw)
		}

		// The provider accepts the refund and returns its id while it is still pending...
		res, err := e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundPending, ProviderRefundID: "rfnd_9"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Refund.ProviderRefundID != "rfnd_9" {
			t.Fatalf("a pending answer must still store the provider id: %+v", res.Refund)
		}
		// ... and it stays when the refund later succeeds.
		if _, err := e.svc.ApplyRefundSnapshot(ctx, r.ID, core.RefundSnapshot{Status: state.RefundSucceeded}); err != nil {
			t.Fatal(err)
		}
		got, err := e.svc.GetRefundByID(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(core.ViewRefund(got))
		v = nil
		_ = json.Unmarshal(raw, &v)
		if v["provider_refund_id"] != "rfnd_9" || v["status"] != "succeeded" {
			t.Errorf("view after success: %s", raw)
		}
	})
}
