// Package metrics is a tiny Prometheus text-format registry with no
// dependencies.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metrics and renders them in Prometheus text format.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
	scrapes  []func()
}

type family struct {
	name, help, typ string
	labels          []string
	mu              sync.Mutex
	series          map[string]*series
	fn              func() float64 // GaugeFunc only
	buckets         []float64      // histograms only: upper bounds, ascending
}

type series struct {
	values []string
	v      float64
	// histograms: per-bucket counts (made cumulative on render), sum and count.
	counts   []float64
	sum, cnt float64
}

// maxSeries bounds the label combinations of one family. A combination beyond
// it is folded into a series whose label values are all "other", so a bug or
// an attacker cannot grow the registry without limit.
const maxSeries = 500

// CounterVec is a counter with labels.
type CounterVec struct{ f *family }

// GaugeVec is a gauge with labels.
type GaugeVec struct{ f *family }

// HistogramVec is a histogram with labels.
type HistogramVec struct{ f *family }

// HTTPBuckets are histogram buckets (seconds) suited to outbound HTTP calls.
var HTTPBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]*family{}} }

func (r *Registry) add(f *family) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[f.name]; dup {
		panic("metrics: duplicate metric " + f.name)
	}
	f.series = map[string]*series{}
	r.families[f.name] = f
	return f
}

// Counter registers a counter family.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	return &CounterVec{r.add(&family{name: name, help: help, typ: "counter", labels: labels})}
}

// Histogram registers a histogram family; buckets are ascending upper bounds
// in the metric's unit (the +Inf bucket is implicit).
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	return &HistogramVec{r.add(&family{name: name, help: help, typ: "histogram", labels: labels, buckets: append([]float64(nil), buckets...)})}
}

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	return &GaugeVec{r.add(&family{name: name, help: help, typ: "gauge", labels: labels})}
}

// GaugeFunc registers an unlabelled gauge whose value is read on each scrape.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, typ: "gauge", fn: fn})
}

// OnScrape registers fn to run before every scrape, to refresh gauges.
func (r *Registry) OnScrape(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scrapes = append(r.scrapes, fn)
}

func (f *family) get(values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	s := f.series[key]
	if s == nil {
		if len(f.series) >= maxSeries {
			values = make([]string, len(f.labels))
			for i := range values {
				values[i] = "other"
			}
			key = strings.Join(values, "\x00")
			if s = f.series[key]; s != nil {
				return s
			}
		}
		s = &series{values: append([]string(nil), values...)}
		if f.typ == "histogram" {
			s.counts = make([]float64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Inc adds 1 to the series.
func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Add adds v to the series.
func (c *CounterVec) Add(v float64, values ...string) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.get(values).v += v
}

// Set sets the series to v. It mirrors a counter kept in durable state (the
// database) rather than in memory; the value should only ever grow.
func (c *CounterVec) Set(v float64, values ...string) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.get(values).v = v
}

// Reset removes all series (before re-mirroring durable state).
func (c *CounterVec) Reset() {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.series = map[string]*series{}
}

// Observe records one observation.
func (h *HistogramVec) Observe(v float64, values ...string) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.f.get(values)
	for i, ub := range h.f.buckets {
		if v <= ub {
			s.counts[i]++
			break
		}
	}
	s.sum += v
	s.cnt++
}

// Set sets the series to v.
func (g *GaugeVec) Set(v float64, values ...string) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	g.f.get(values).v = v
}

// Reset removes all series.
func (g *GaugeVec) Reset() {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	g.f.series = map[string]*series{}
}

// Handler serves the registry in Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		scrapes := append([]func(){}, r.scrapes...)
		r.mu.Unlock()
		for _, fn := range scrapes {
			fn()
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.render()))
	})
}

func (r *Registry) render() string {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })

	var b strings.Builder
	for _, f := range fams {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.typ)
		if f.fn != nil {
			fmt.Fprintf(&b, "%s %s\n", f.name, fmtFloat(f.fn()))
			continue
		}
		f.mu.Lock()
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			labels := func(extra string) string {
				var l strings.Builder
				for i, name := range f.labels {
					if i > 0 {
						l.WriteByte(',')
					}
					fmt.Fprintf(&l, `%s="%s"`, name, escapeLabel(s.values[i]))
				}
				if extra != "" {
					if l.Len() > 0 {
						l.WriteByte(',')
					}
					l.WriteString(extra)
				}
				if l.Len() == 0 {
					return ""
				}
				return "{" + l.String() + "}"
			}
			if f.typ == "histogram" {
				cum := 0.0
				for i, ub := range f.buckets {
					cum += s.counts[i]
					fmt.Fprintf(&b, "%s_bucket%s %s\n", f.name, labels(`le="`+fmtFloat(ub)+`"`), fmtFloat(cum))
				}
				fmt.Fprintf(&b, "%s_bucket%s %s\n", f.name, labels(`le="+Inf"`), fmtFloat(s.cnt))
				fmt.Fprintf(&b, "%s_sum%s %s\n%s_count%s %s\n", f.name, labels(""), fmtFloat(s.sum), f.name, labels(""), fmtFloat(s.cnt))
				continue
			}
			fmt.Fprintf(&b, "%s%s %s\n", f.name, labels(""), fmtFloat(s.v))
		}
		f.mu.Unlock()
	}
	return b.String()
}

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeLabel(s string) string { return labelEscaper.Replace(s) }
