package whop

import (
	"encoding/json"
	"testing"
)

// Whop returns some amounts as a money object (seen on a sandbox payment,
// 2026-10-17): {"currency":"usd","amount":"10.00","decimals":2,...}.
func TestNumberAcceptsMoneyObject(t *testing.T) {
	cases := map[string]number{
		`"10.00"`: "10.00",
		`10.5`:    "10.5",
		`null`:    "",
		`{"currency":"usd","amount":"10.00","decimals":2,"display_decimals":2}`: "10.00",
		`{"currency":"usd","amount":12.34}`:                                     "12.34",
	}
	for in, want := range cases {
		var n number
		if err := json.Unmarshal([]byte(in), &n); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if n != want {
			t.Errorf("%s: got %q want %q", in, n, want)
		}
	}
	if v, err := minor(number("10.00"), "USD"); err != nil || v != 1000 {
		t.Fatalf("minor: %d %v", v, err)
	}
}
