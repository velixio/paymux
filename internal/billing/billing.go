// Package billing holds the pure calendar and policy math for subscriptions:
// period boundaries, pre-debit timing, UPI execution windows, dunning and
// mandate limits. It has no I/O.
package billing

import (
	"errors"
	"fmt"
	"time"
)

// Interval is a subscription billing unit.
type Interval string

const (
	Day   Interval = "day"
	Week  Interval = "week"
	Month Interval = "month"
	Year  Interval = "year"
)

func daysIn(y int, m time.Month, loc *time.Location) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
}

// Boundary returns the start of period k (k=0 is the anchor). Month and year
// periods keep the anchor's day of month, clamped to the month's last day, so
// a Jan 31 anchor renews Feb 28/29, then Mar 31 (no drift).
func Boundary(anchor time.Time, iv Interval, count, k int) time.Time {
	switch iv {
	case Day:
		return anchor.AddDate(0, 0, count*k)
	case Week:
		return anchor.AddDate(0, 0, 7*count*k)
	}
	months := count * k
	if iv == Year {
		months *= 12
	}
	loc := anchor.Location()
	total := int(anchor.Month()) - 1 + months
	y := anchor.Year() + total/12
	m := time.Month(total%12 + 1)
	if total < 0 && total%12 != 0 {
		y--
		m = time.Month(total%12 + 13)
	}
	d := min(anchor.Day(), daysIn(y, m, loc))
	h, mi, s := anchor.Clock()
	return time.Date(y, m, d, h, mi, s, anchor.Nanosecond(), loc)
}

// PeriodContaining returns the period [start, end) that contains t, and its index.
func PeriodContaining(anchor time.Time, iv Interval, count int, t time.Time) (start, end time.Time, k int) {
	if t.Before(anchor) {
		return anchor, Boundary(anchor, iv, count, 1), 0
	}
	approx := map[Interval]time.Duration{Day: 24 * time.Hour, Week: 7 * 24 * time.Hour, Month: 28 * 24 * time.Hour, Year: 365 * 24 * time.Hour}[iv]
	k = int(t.Sub(anchor) / (approx * time.Duration(count)))
	for k > 0 && Boundary(anchor, iv, count, k).After(t) {
		k--
	}
	for !Boundary(anchor, iv, count, k+1).After(t) {
		k++
	}
	return Boundary(anchor, iv, count, k), Boundary(anchor, iv, count, k+1), k
}

// MinNoticeGap is the RBI minimum between pre-debit notice and debit.
const MinNoticeGap = 24 * time.Hour

// PreDebitAt is when to send the pre-debit notice for a charge.
func PreDebitAt(chargeAt time.Time, lead time.Duration) time.Time { return chargeAt.Add(-lead) }

// ChargeAllowed reports whether a debit may run at now given when the notice
// was sent; if not, it returns the earliest allowed time.
func ChargeAllowed(notifiedAt, now time.Time) (bool, time.Time) {
	return ChargeAllowedAfter(notifiedAt, now, MinNoticeGap)
}

// NoticeGap is the least time between a notice and its debit for a provider
// that asks for minHours (Info.PreDebitMinHours): never under the RBI's 24h.
func NoticeGap(minHours int) time.Duration {
	if g := time.Duration(minHours) * time.Hour; g > MinNoticeGap {
		return g
	}
	return MinNoticeGap
}

// ChargeAllowedAfter is ChargeAllowed for a provider that wants a notice gap
// longer than the RBI's (see NoticeGap).
func ChargeAllowedAfter(notifiedAt, now time.Time, gap time.Duration) (bool, time.Time) {
	earliest := notifiedAt.Add(gap)
	if now.Before(earliest) {
		return false, earliest
	}
	return true, now
}

// Window is a daily [From, To) range in minutes after IST midnight.
type Window struct{ From, To int }

var istLoc = time.FixedZone("IST", 5*3600+1800)

// DefaultUPIPeakHours are NPCI OC-215A peak windows when AutoPay must not execute.
var DefaultUPIPeakHours = []Window{{10 * 60, 13 * 60}, {17 * 60, 21*60 + 30}}

// SnapOutsidePeak moves t (interpreted in IST) to the end of any peak window it falls in.
func SnapOutsidePeak(t time.Time, peaks []Window) time.Time {
	for moved := true; moved; {
		moved = false
		lt := t.In(istLoc)
		mins := lt.Hour()*60 + lt.Minute()
		for _, w := range peaks {
			if mins >= w.From && mins < w.To {
				y, m, d := lt.Date()
				t = time.Date(y, m, d, w.To/60, w.To%60, 0, 0, istLoc)
				moved = true
			}
		}
	}
	return t
}

// Method is a mandate payment method.
type Method string

const (
	MethodUPI      Method = "upi"
	MethodCard     Method = "card"
	MethodEMandate Method = "emandate"
)

// MaxUPIRetries is NPCI's cap: 1 attempt + 3 retries per execution.
const MaxUPIRetries = 3

// DefaultDunning retries a failed renewal after 1, 3 and 5 days.
var DefaultDunning = []time.Duration{24 * time.Hour, 3 * 24 * time.Hour, 5 * 24 * time.Hour}

// RetrySchedule applies per-method caps to a configured schedule.
func RetrySchedule(s []time.Duration, m Method) []time.Duration {
	if m == MethodUPI && len(s) > MaxUPIRetries {
		return s[:MaxUPIRetries]
	}
	return s
}

// ValidateDunning enforces §8a limits: 0–6 retries, each 1h–30d.
func ValidateDunning(s []time.Duration) error {
	if len(s) > 6 {
		return fmt.Errorf("billing: at most 6 retries, got %d", len(s))
	}
	for i, d := range s {
		if d < time.Hour || d > 30*24*time.Hour {
			return fmt.Errorf("billing: retry %d must be between 1h and 30d, got %v", i+1, d)
		}
	}
	return nil
}

// ErrMandateLimit means the amount exceeds what the mandate method allows.
var ErrMandateLimit = errors.New("billing: amount exceeds mandate limit")

// MandateMax returns the mandate max amount: amount × headroom (basis points,
// 10000 = 1.0×), capped at limit, never below amount.
func MandateMax(amount int64, headroomBP int64, limit int64) (int64, error) {
	if amount > limit {
		return 0, ErrMandateLimit
	}
	if headroomBP < 10000 {
		headroomBP = 10000
	}
	return min(amount*headroomBP/10000, limit), nil
}
