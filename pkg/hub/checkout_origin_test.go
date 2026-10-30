package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/logging"
)

// B-7c: with PAYMUX_CHECKOUT_URL the customer pages live on a host of their own.
func TestCheckoutOrigin(t *testing.T) {
	dir := t.TempDir()
	seed := seedSim(t, "sqlite://"+dir+"/paymux.db", dir, "http://127.0.0.1:1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]
	ln.Close()
	cfg := testConfig(t, "sqlite://"+dir+"/paymux.db", dir)
	cfg.Listen = addr
	cfg.BaseURL, _ = neturl.Parse("http://dash.localhost:" + port)
	cfg.CheckoutURL, _ = neturl.Parse("http://pay.localhost:" + port)
	logs := &syncBuf{}
	app, err := New(cfg, WithLogger(logging.New(logs, -4)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Start(ctx) }()
	r := &running{app: app, logs: logs, done: make(chan struct{}), stop: cancel}
	go func() { r.err = <-done; close(r.done) }()
	waitReady(t, r)
	defer shutdown(t, r)

	const dash, pay = "dash.localhost:", "pay.localhost:"
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(host, method, path string, body io.Reader, hdr map[string]string) (int, http.Header, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+addr+path, body)
		req.Host = host + port
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, string(b)
	}
	apiHdr := map[string]string{"Authorization": "Bearer " + seed.apiKey, "Content-Type": "application/json"}
	api := func(path string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		code, _, out := do(dash, "POST", path, bytes.NewReader(b), apiHdr)
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		if code != 201 {
			t.Fatalf("POST %s: %d %s", path, code, out)
		}
		return m
	}

	// Every customer URL the API hands out is on the checkout host.
	pmt := api("/v1/payments", map[string]any{"amount": 49900, "currency": "INR", "order_ref": "co-1", "return_url": "https://shop.example.com/thanks",
		"customer": map[string]any{"external_ref": "u1", "name": "Asha", "email": "asha@example.com", "billing": map[string]any{"state_code": "29", "country": "IN"}}})
	checkout, _ := neturl.Parse(pmt["checkout_url"].(string))
	if checkout.Host != "pay.localhost:"+port || !strings.HasPrefix(checkout.Path, "/pay/") {
		t.Fatalf("checkout_url %q", pmt["checkout_url"])
	}
	link := api("/v1/payment_links", map[string]any{"amount": 1000, "currency": "INR", "description": "Spring sale"})
	if u, _ := link["url"].(string); !strings.HasPrefix(u, "http://pay.localhost:"+port+"/l/") {
		t.Errorf("payment link url %v", link)
	}

	// The base host sends the customer pages there (308, path and query kept).
	code, hdr, _ := do(dash, "GET", checkout.RequestURI(), nil, nil)
	if code != http.StatusPermanentRedirect || hdr.Get("Location") != pmt["checkout_url"] {
		t.Errorf("base host: %d %q, want 308 %q", code, hdr.Get("Location"), pmt["checkout_url"])
	}
	// On the checkout host the pay page works, and the simulator's page is on
	// the checkout host too.
	code, hdr, body := do(pay, "GET", checkout.RequestURI(), nil, nil)
	sim := hdr.Get("Location")
	if code != 303 || !strings.HasPrefix(sim, "http://pay.localhost:"+port+"/sim/") {
		t.Fatalf("pay page: %d %q %s", code, sim, body)
	}
	simURL, _ := neturl.Parse(sim)
	if code, _, body := do(pay, "GET", simURL.RequestURI(), nil, nil); code != 200 || !strings.Contains(body, "Succeed") {
		t.Errorf("simulator page on the checkout host: %d", code)
	}
	if code, _, _ := do(pay, "GET", "/assets/favicon.svg", nil, nil); code != 200 {
		t.Errorf("assets on the checkout host: %d", code)
	}

	// Nothing of the dashboard, the API or the hooks exists on the checkout host.
	for _, c := range []struct{ method, path string }{{"GET", "/dashboard/"}, {"GET", "/dashboard/login"}, {"POST", "/dashboard/login"}, {"GET", "/setup"},
		{"GET", "/v1/payments"}, {"POST", "/v1/payments"}, {"POST", "/hooks/pa_x"}, {"GET", "/docs"}, {"GET", "/"}, {"GET", "/metrics"}} {
		code, hdr, _ := do(pay, c.method, c.path, nil, apiHdr)
		if code != 404 || len(hdr.Values("Set-Cookie")) != 0 {
			t.Errorf("%s %s on the checkout host: %d, cookies %v", c.method, c.path, code, hdr.Values("Set-Cookie"))
		}
	}
	// The dashboard stays on the base host, and its cookies are host-only.
	cookies := 0
	for _, path := range []string{"/dashboard/login", "/setup", "/dashboard/"} {
		code, hdr, _ := do(dash, "GET", path, nil, nil)
		if code == 404 {
			t.Errorf("%s on the base host: 404", path)
		}
		for _, c := range hdr.Values("Set-Cookie") {
			cookies++
			if strings.Contains(strings.ToLower(c), "domain=") {
				t.Errorf("cookie with a Domain attribute: %q", c)
			}
		}
	}
	if cookies == 0 {
		t.Log("no dashboard cookie set by these pages; the Domain check is vacuous here")
	}
}
