package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	cspReportMax    = 4 << 10 // bytes: a real report is a few hundred
	cspReportLimit  = 20      // reports per address per window
	cspReportWindow = time.Minute
)

// CSPReportOptions configures NewCSPReports.
type CSPReportOptions struct {
	Logger *slog.Logger
	// Count is called once per accepted report with the violated directive
	// (one of a fixed set, else "other"), for a metric.
	Count func(directive string)
	// ClientIP names the sender for the rate limit (default: the connection's address).
	ClientIP func(*http.Request) string
	Now      func() time.Time
}

// cspDirectives are the labels a report can count under; anything else is
// "other", so a hostile page cannot grow the metric without bound.
var cspDirectives = map[string]bool{"default-src": true, "script-src": true, "script-src-elem": true, "script-src-attr": true,
	"style-src": true, "style-src-elem": true, "style-src-attr": true, "img-src": true, "frame-src": true, "connect-src": true,
	"form-action": true, "base-uri": true, "frame-ancestors": true, "font-src": true, "object-src": true, "media-src": true}

// cspKeywords are the blocked-uri values that name no URL.
var cspKeywords = map[string]bool{"inline": true, "eval": true, "data": true, "blob": true, "self": true, "about": true, "wasm-eval": true}

// CSPReports receives the violation reports browsers send to the report-uri of
// the hosted pages' policy (POST /csp-report). It keeps nothing but a count and
// a log line with the directive and the blocked origin: the page URL and the
// query strings of the blocked URL can carry payment secrets, so they are never
// read past parsing.
type CSPReports struct {
	o       CSPReportOptions
	mu      sync.Mutex
	windows map[string]*reportWindow
}

type reportWindow struct {
	start time.Time
	n     int
}

// NewCSPReports builds the handler.
func NewCSPReports(o CSPReportOptions) *CSPReports {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ClientIP == nil {
		o.ClientIP = func(r *http.Request) string {
			h, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				return r.RemoteAddr
			}
			return h
		}
	}
	return &CSPReports{o: o, windows: map[string]*reportWindow{}}
}

// allow counts one report of ip in its window.
func (c *CSPReports) allow(ip string) bool {
	now := c.o.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.windows) > 10000 { // a flood of addresses must not grow the map
		for k, w := range c.windows {
			if now.Sub(w.start) >= cspReportWindow {
				delete(c.windows, k)
			}
		}
	}
	w := c.windows[ip]
	if w == nil || now.Sub(w.start) >= cspReportWindow {
		w = &reportWindow{start: now}
		c.windows[ip] = w
	}
	w.n++
	return w.n <= cspReportLimit
}

func (c *CSPReports) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !c.allow(c.o.ClientIP(r)) {
		http.Error(w, "too many reports", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, cspReportMax+1))
	if err != nil || len(body) > cspReportMax {
		http.Error(w, "report too large", http.StatusRequestEntityTooLarge)
		return
	}
	var doc struct {
		Report struct {
			Effective string `json:"effective-directive"`
			Violated  string `json:"violated-directive"`
			Blocked   string `json:"blocked-uri"`
		} `json:"csp-report"`
	}
	_ = json.Unmarshal(body, &doc) // a malformed report is counted as "other"
	directive, _, _ := strings.Cut(strings.TrimSpace(doc.Report.Effective+" "+doc.Report.Violated), " ")
	if !cspDirectives[directive] {
		directive = "other"
	}
	blocked := blockedOrigin(doc.Report.Blocked)
	if c.o.Count != nil {
		c.o.Count(directive)
	}
	c.o.Logger.Warn("csp: violation reported", "directive", directive, "blocked", blocked)
	w.WriteHeader(http.StatusNoContent)
}

// blockedOrigin reduces a blocked-uri to scheme://host[:port], or one of the
// keywords browsers use when there is no URL.
func blockedOrigin(raw string) string {
	if cspKeywords[raw] {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "other"
	}
	return u.Scheme + "://" + strings.ToLower(u.Host)
}
