package whop

import (
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func threeDSOf(t *testing.T, field string) string {
	t.Helper()
	p := decodePayment(t, `{"id":"pay_1","status":"open","total":4.99,"currency":"usd"`+field+`}`)
	s, err := p.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return s.ThreeDS
}

func TestThreeDSTrueIsPassed(t *testing.T) {
	if got := threeDSOf(t, `,"three_ds_verified":true`); got != connector.ThreeDSPassed {
		t.Fatalf("got %q", got)
	}
}

func TestThreeDSFalseIsNotVerified(t *testing.T) {
	if got := threeDSOf(t, `,"three_ds_verified":false`); got != connector.ThreeDSNotVerified {
		t.Fatalf("got %q; false never means failed", got)
	}
}

func TestThreeDSAbsentIsEmpty(t *testing.T) {
	if got := threeDSOf(t, ``); got != "" {
		t.Fatalf("got %q", got)
	}
}
