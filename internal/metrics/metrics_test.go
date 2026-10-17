package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(r *Registry) string {
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestMetricsTextFormat(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("zz_requests_total", "Requests.\nSecond line \\ here", "route", "code")
	c.Inc("/b", "200")
	c.Inc("/a", "500")
	c.Add(2, "/a", "500")
	g := r.Gauge("aa_info", "Info.", "version")
	g.Set(1, `v"1\x`+"\n")
	r.GaugeFunc("mm_value", "A value.", func() float64 { return 1.5 })

	want := `# HELP aa_info Info.
# TYPE aa_info gauge
aa_info{version="v\"1\\x\n"} 1
# HELP mm_value A value.
# TYPE mm_value gauge
mm_value 1.5
# HELP zz_requests_total Requests.\nSecond line \\ here
# TYPE zz_requests_total counter
zz_requests_total{route="/a",code="500"} 3
zz_requests_total{route="/b",code="200"} 1
`
	if got := scrape(r); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestGaugeResetAndOnScrape(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("depth", "d", "state")
	calls := 0
	r.OnScrape(func() { calls++; g.Reset(); g.Set(float64(calls), "pending") })
	scrape(r)
	out := scrape(r)
	if !strings.Contains(out, `depth{state="pending"} 2`) || strings.Count(out, "depth{") != 1 {
		t.Fatal(out)
	}
}

func TestContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRegistry().Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatal(ct)
	}
}
