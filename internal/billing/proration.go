package billing

import (
	"math/big"
	"time"
)

// Prorate returns the signed amount to add for changing a subscription's
// per-period amount from oldAmount to newAmount at time at, inside the period
// [start, end): (newAmount - oldAmount) x remaining / length, with remaining
// and length in whole seconds, rounded to the nearest minor unit (halves away
// from zero). It is positive for an upgrade (extra to charge), negative for a
// downgrade (credit) and zero outside the period, which has nothing left to
// prorate. The product uses big integers, so no amount overflows.
func Prorate(oldAmount, newAmount int64, start, end, at time.Time) int64 {
	length := int64(end.Sub(start) / time.Second)
	if length <= 0 || !at.Before(end) {
		return 0
	}
	remaining := int64(end.Sub(at) / time.Second)
	if at.Before(start) { // a change before the period starts covers all of it
		remaining = length
	}
	num := new(big.Int).Mul(big.NewInt(newAmount-oldAmount), big.NewInt(remaining))
	den := big.NewInt(length)
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// QuoRem truncates toward zero: round half away from zero.
	if new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2)).Cmp(den) >= 0 {
		if num.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q.Int64()
}
