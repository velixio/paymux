package checkout_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/core/state"
	"github.com/paymux/paymux/pkg/connector"
)

func sdkAction(order string) connector.CheckoutResult {
	return connector.CheckoutResult{ProviderOrderID: order, NextAction: connector.NextAction{Kind: connector.ActionSDK,
		ScriptURL: "https://fake.example.com/checkout.js", Options: map[string]any{"order_id": order, "key": "k"}}}
}

// sdkCheckouts makes the fake create a distinct provider order per call.
func (e *env) sdkCheckouts(expiresAt func() int64) {
	n := 0
	e.fake.Locked(func() {
		e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
			n++
			res := sdkAction("order_" + string(rune('A'+n-1)))
			if expiresAt != nil {
				res.ExpiresAt = expiresAt()
			}
			return res, nil
		}
	})
}

func (e *env) checkoutCalls() int {
	var n int
	e.fake.Locked(func() { n = len(e.fake.Checkouts) })
	return n
}

func TestReloadingThePayPageReusesTheOpenAttempt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.sdkCheckouts(nil)
		p := e.payment2()
		first, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		second, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if second.Attempt.ID != first.Attempt.ID || second.AccountID != first.AccountID || second.Connector != first.Connector {
			t.Errorf("a reload made a new attempt: %s then %s", first.Attempt.ID, second.Attempt.ID)
		}
		if !reflect.DeepEqual(second.Action, first.Action) {
			t.Errorf("the stored next action differs:\n%+v\n%+v", first.Action, second.Action)
		}
		if second.Attempt.ProviderOrderID != "order_A" || e.checkoutCalls() != 1 {
			t.Errorf("order %q, %d provider checkouts (want 1)", second.Attempt.ProviderOrderID, e.checkoutCalls())
		}
		if n := len(e.attempts(p.ID)); n != 1 {
			t.Errorf("%d attempts", n)
		}
	})
}

func TestAFailedAttemptIsNotReused(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.sdkCheckouts(nil)
		p := e.payment2()
		first, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.core.ApplyAttemptSnapshot(ctx, first.Attempt.ID, core.Snapshot{Status: state.AttemptFailed, FailureCode: "declined"}); err != nil {
			t.Fatal(err)
		}
		second, err := e.flow.StartCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if second.Attempt.ID == first.Attempt.ID || e.checkoutCalls() != 2 {
			t.Errorf("a failed attempt was reused: %s / %s (%d checkouts)", first.Attempt.ID, second.Attempt.ID, e.checkoutCalls())
		}
	})
}

func TestAnAttemptIsReusedUntilItsCheckoutExpires(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		// No expiry from the provider: reused for 15 minutes.
		e.sdkCheckouts(nil)
		p := e.payment2()
		first, _ := e.flow.StartCheckout(ctx, p.ID)
		e.clk.Add(14 * time.Minute)
		if again, _ := e.flow.StartCheckout(ctx, p.ID); again.Attempt.ID != first.Attempt.ID {
			t.Error("not reused inside the default window")
		}
		e.clk.Add(2 * time.Minute)
		if later, err := e.flow.StartCheckout(ctx, p.ID); err != nil || later.Attempt.ID == first.Attempt.ID {
			t.Errorf("reused past the default window: %v %+v", err, later.Attempt)
		}

		// An expiry from the provider wins, in either direction.
		e2 := e.payment2()
		now := e.clk.Now()
		e.sdkCheckouts(func() int64 { return now.Add(40 * time.Minute).UnixMilli() })
		long, _ := e.flow.StartCheckout(ctx, e2.ID)
		e.clk.Add(30 * time.Minute)
		if again, _ := e.flow.StartCheckout(ctx, e2.ID); again.Attempt.ID != long.Attempt.ID {
			t.Error("a 40 minute provider expiry was cut to the default window")
		}
		e.clk.Add(11 * time.Minute)
		if later, _ := e.flow.StartCheckout(ctx, e2.ID); later.Attempt.ID == long.Attempt.ID {
			t.Error("reused after the provider's expiry")
		}
	})
}

func TestFreshCheckoutStartsANewAttemptEvenWhileOneIsOpen(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		e.sdkCheckouts(nil)
		p := e.payment2()
		first, _ := e.flow.StartCheckout(ctx, p.ID)
		e.clk.Add(time.Second) // attempts are ordered by creation time; the test clock is frozen
		fresh, err := e.flow.StartFreshCheckout(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Attempt.ID == first.Attempt.ID || fresh.Attempt.ProviderOrderID != "order_B" || e.checkoutCalls() != 2 {
			t.Errorf("fresh checkout: %+v (%d checkouts)", fresh.Attempt, e.checkoutCalls())
		}
		// The next plain load reuses the newest open attempt.
		third, _ := e.flow.StartCheckout(ctx, p.ID)
		if third.Attempt.ID != fresh.Attempt.ID {
			t.Errorf("a reload after a fresh start should show the fresh attempt: %s vs %s", third.Attempt.ID, fresh.Attempt.ID)
		}
		// The earlier attempt is still open: it can still be paid.
		if a, _ := e.core.GetAttempt(ctx, first.Attempt.ID); a.Status == string(state.AttemptFailed) {
			t.Error("the earlier attempt was failed by a fresh start")
		}
	})
}
