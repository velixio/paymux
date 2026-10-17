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
}

type series struct {
	values []string
	v      float64
}

// CounterVec is a counter with labels.
type CounterVec struct{ f *family }

// GaugeVec is a gauge with labels.
type GaugeVec struct{ f *family }

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
		s = &series{values: append([]string(nil), values...)}
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
			b.WriteString(f.name)
			if len(f.labels) > 0 {
				b.WriteByte('{')
				for i, l := range f.labels {
					if i > 0 {
						b.WriteByte(',')
					}
					fmt.Fprintf(&b, `%s="%s"`, l, escapeLabel(s.values[i]))
				}
				b.WriteByte('}')
			}
			fmt.Fprintf(&b, " %s\n", fmtFloat(s.v))
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
