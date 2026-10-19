//go:build unix && !race

package redact

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// cpuTime is the process's user+system CPU time. Unlike wall-clock time it
// does not grow when other test binaries share a busy CI runner, so the
// scaling check below measures the redactor and not the machine.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// Redaction stays linear on hostile input. This runs WITHOUT the race detector
// (see scripts/ci.sh): -race distorts memory and GC behaviour so much that its
// timings say nothing about the code (measured: x2.0 per doubling without it,
// erratic with it). Four times the input may take at
// most about four times the CPU (a quadratic path would take sixteen), so a
// ceiling of ten catches a super-linear path without flaking on noise.
func TestHostileInputIsLinear(t *testing.T) {
	r := New()
	r.SetKnown("a", "KNOWNSECRETVALUE")
	type gen func(n int) string
	for name, g := range map[string]gen{
		"colons":  func(n int) string { return strings.Repeat("a:", n) },
		"form":    func(n int) string { return strings.Repeat("a=b&", n/2) },
		"digits+": func(n int) string { return strings.Repeat("1 ", n) },
		"json":    func(n int) string { return strings.Repeat(`{"a":`, n/6) },
		"digits":  func(n int) string { return strings.Repeat("4", 2*n) },
		"pem":     func(n int) string { return strings.Repeat("-----BEGIN X-----", n/16) },
	} {
		cost := func(in string) time.Duration {
			best := time.Duration(1<<63 - 1)
			for range 5 { // the cheapest of five runs: noise only adds
				before := cpuTime()
				r.Body("", []byte(in))
				r.Text(in)
				best = min(best, cpuTime()-before)
			}
			return best
		}
		small, large := cost(g(8<<10)), cost(g(32<<10))
		if small < 2*time.Millisecond {
			small = 2 * time.Millisecond // below the clock's useful resolution: compare against a floor
		}
		if large > 10*small {
			t.Errorf("%s: 4x the input took %.1fx the CPU (%v vs %v): not linear", name, float64(large)/float64(small), large, small)
		}
	}
}
