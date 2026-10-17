package stripe

import (
	"fmt"
	"math"
	"strings"

	"github.com/paymux/paymux/pkg/connector/money"
)

// Stripe's amounts are integers in the currency's smallest unit, but its idea of
// that unit differs from ISO 4217 for a few currencies:
//
//   - zero-decimal currencies are sent as whole units (JPY 500 is 500). Stripe's
//     list also has MGA, which ISO gives two decimals;
//   - ISK and UGX are zero-decimal in ISO but Stripe wants them as two-decimal
//     (ISK 5 is sent as 500, and the cents must be 00);
//   - three-decimal currencies (BHD JOD KWD OMR TND) are sent with three decimals
//     but the last digit must be 0, because Stripe settles them to two.
//
// Paymux keeps ISO minor units everywhere; these functions are the only place
// that knows about the difference.
//
// INR, USD, EUR, JPY and HUF were accepted by the live Checkout. TODO(spec):
// re-check the list against https://docs.stripe.com/currencies when adding a
// currency; HUF and TWD are two-decimal for charges.
var (
	stripeZero  = fields("BIF CLP DJF GNF JPY KMF KRW MGA PYG RWF VND VUV XAF XOF XPF")
	stripeThree = fields("BHD JOD KWD OMR TND")
)

func fields(s string) map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(s) {
		m[c] = true
	}
	return m
}

// stripeExponent is the number of decimals Stripe's integer amounts of the
// currency carry.
func stripeExponent(cur string) int {
	switch {
	case stripeZero[cur]:
		return 0
	case stripeThree[cur]:
		return 3
	}
	return 2
}

func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// toStripe converts ISO minor units to Stripe's integer for the currency.
func toStripe(minor int64, cur string) (int64, error) {
	cur = strings.ToUpper(cur)
	oe, err := money.Exponent(cur)
	if err != nil {
		return 0, fmt.Errorf("stripe: unsupported currency %q", cur)
	}
	se := stripeExponent(cur)
	v := minor
	switch {
	case se > oe:
		f := pow10(se - oe)
		if minor > math.MaxInt64/f {
			return 0, fmt.Errorf("stripe: amount %d %s is too large", minor, cur)
		}
		v = minor * f
	case se < oe:
		f := pow10(oe - se)
		if minor%f != 0 {
			return 0, fmt.Errorf("stripe: %s amounts must be whole units (got %d minor units)", cur, minor)
		}
		v = minor / f
	}
	if se == 3 && v%10 != 0 {
		return 0, fmt.Errorf("stripe: %s amounts must be multiples of 0.010 (got %d)", cur, minor)
	}
	return v, nil
}

// fromStripe converts Stripe's integer for the currency to ISO minor units.
// Unknown currencies are passed through unchanged.
func fromStripe(v int64, cur string) int64 {
	if v < 0 {
		return -fromStripe(-v, cur)
	}
	cur = strings.ToUpper(cur)
	oe, err := money.Exponent(cur)
	if err != nil {
		return v
	}
	se := stripeExponent(cur)
	switch {
	case se > oe:
		f := pow10(se - oe)
		return (v + f/2) / f // round to the nearest unit
	case se < oe:
		return v * pow10(oe-se)
	}
	return v
}
