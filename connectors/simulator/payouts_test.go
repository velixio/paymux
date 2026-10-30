package simulator

import (
	"errors"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func payoutReq(ref string, amount int64) connector.PayoutRequest {
	return connector.PayoutRequest{ProviderRef: ref, Amount: amount, Currency: "INR", Narration: "test",
		Beneficiary: connector.Beneficiary{Name: "Asha Rao", AccountNumber: "123456789012", IFSC: "HDFC0001234"}}
}

func TestSimulatorIsAPayouter(t *testing.T) {
	h := newHarness(t, `{}`)
	var _ connector.Payouter = h.c
	found := false
	for _, c := range h.c.Info().Capabilities {
		found = found || c == connector.CapPayout
	}
	if !found {
		t.Error("the simulator does not declare the payout capability")
	}
}

// With no delay a payout is paid at once and the provider says so by webhook.
func TestPayoutSucceedsAtOnceAndIsIdempotent(t *testing.T) {
	h := newHarness(t, `{}`)
	snap, err := h.c.CreatePayout(ctx, payoutReq("po_a", 5000))
	if err != nil || snap.Status != connector.PayoutSucceeded || snap.ProviderRef != "po_a" || snap.ProviderPayoutID == "" || snap.Amount != 5000 {
		t.Fatalf("payout %+v %v", snap, err)
	}
	n := h.next("payout.processed")
	if n.Kind != connector.NotifyPayout || n.Payout == nil || n.Payout.ProviderRef != "po_a" || n.Payout.ProviderPayoutID != snap.ProviderPayoutID {
		t.Errorf("webhook %+v", n)
	}
	// The same reference is the same payout.
	again, err := h.c.CreatePayout(ctx, payoutReq("po_a", 5000))
	if err != nil || again.ProviderPayoutID != snap.ProviderPayoutID {
		t.Errorf("repeat %+v %v", again, err)
	}
	h.noMoreHooks(100 * 1e6)
	for _, ref := range []connector.PayoutRef{{ProviderRef: "po_a"}, {ProviderPayoutID: snap.ProviderPayoutID}} {
		got, err := h.c.FetchPayout(ctx, ref)
		if err != nil || got.Status != connector.PayoutSucceeded || got.Amount != 5000 {
			t.Errorf("FetchPayout(%+v) = %+v %v", ref, got, err)
		}
	}
	if _, err := h.c.FetchPayout(ctx, connector.PayoutRef{ProviderRef: "nope"}); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown payout: %v", err)
	}
	for _, bad := range []connector.PayoutRequest{payoutReq("", 100), payoutReq("po_b", 0), payoutReq("po_b", -5)} {
		if _, err := h.c.CreatePayout(ctx, bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

// With a delay the payout waits; the dashboard (Controller) decides how it ends.
func TestPayoutOutcomesAreDrivenByTheController(t *testing.T) {
	h := newHarness(t, `{"payout_delay_seconds":3600}`)
	snap, err := h.c.CreatePayout(ctx, payoutReq("po_slow", 700))
	if err != nil || snap.Status != connector.PayoutProcessing {
		t.Fatalf("payout %+v %v", snap, err)
	}
	h.noMoreHooks(50 * 1e6)

	ok, err := h.c.SimulatePayout(ctx, "po_slow", PayoutSucceed)
	if err != nil || ok.Status != connector.PayoutSucceeded {
		t.Fatalf("succeed %+v %v", ok, err)
	}
	h.next("payout.processed")
	rev, err := h.c.SimulatePayout(ctx, "po_slow", PayoutReverse)
	if err != nil || rev.Status != connector.PayoutReversed {
		t.Fatalf("reverse %+v %v", rev, err)
	}
	if n := h.next("payout.reversed"); n.Kind != connector.NotifyPayout || n.Payout.ProviderRef != "po_slow" {
		t.Errorf("webhook %+v", n)
	}
	if got, _ := h.c.FetchPayout(ctx, connector.PayoutRef{ProviderRef: "po_slow"}); got.Status != connector.PayoutReversed {
		t.Errorf("fetched %+v", got)
	}
	// A reversed payout does not move again.
	if _, err := h.c.SimulatePayout(ctx, "po_slow", PayoutSucceed); err == nil {
		t.Error("a reversed payout was paid")
	}

	if _, err := h.c.CreatePayout(ctx, payoutReq("po_bad", 900)); err != nil {
		t.Fatal(err)
	}
	failed, err := h.c.SimulatePayout(ctx, "po_bad", PayoutFail)
	if err != nil || failed.Status != connector.PayoutFailed || failed.ErrorCode == "" {
		t.Fatalf("fail %+v %v", failed, err)
	}
	h.next("payout.failed")
	if _, err := h.c.SimulatePayout(ctx, "po_missing", PayoutFail); !errors.Is(err, connector.ErrNotFound) {
		t.Errorf("unknown payout: %v", err)
	}
}
