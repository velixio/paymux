package metrics

import (
	"fmt"
	"strings"
	"testing"
)

func TestHistogramRendering(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("req_seconds", "Latency.", []float64{0.1, 1}, "connector")
	h.Observe(0.05, "x")
	h.Observe(0.5, "x")
	h.Observe(0.7, "x")
	h.Observe(5, "x")
	want := `# HELP req_seconds Latency.
# TYPE req_seconds histogram
req_seconds_bucket{connector="x",le="0.1"} 1
req_seconds_bucket{connector="x",le="1"} 3
req_seconds_bucket{connector="x",le="+Inf"} 4
req_seconds_sum{connector="x"} 6.25
req_seconds_count{connector="x"} 4
`
	if got := scrape(r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	u := NewRegistry()
	u.Histogram("u_seconds", "U.", []float64{1}).Observe(2)
	if out := scrape(u); !strings.Contains(out, `u_seconds_bucket{le="1"} 0`) || !strings.Contains(out, "u_seconds_sum 2\n") {
		t.Fatal(out)
	}
}

func TestCounterSetAndSeriesCap(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "C.", "id")
	c.Set(7, "a")
	c.Set(9, "a")
	for i := 0; i < maxSeries+50; i++ {
		c.Inc(fmt.Sprint("id", i))
	}
	out := scrape(r)
	if !strings.Contains(out, `c_total{id="a"} 9`) || !strings.Contains(out, `c_total{id="other"} `) || strings.Count(out, "c_total{") > maxSeries+1 {
		t.Fatalf("series not capped: %d lines", strings.Count(out, "c_total{"))
	}
}
