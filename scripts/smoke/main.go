// Command smoke drives a running Paymux, from first-run setup to a refund, with
// the simulator.
//
// It is used by scripts/smoke.sh and needs only the Go standard library. Every
// step either passes or stops the run with a message that says what was expected
// and what happened.
//
//	go run ./scripts/smoke -base http://127.0.0.1:8080 -code abcdef-ghijkl-mnopqr-stuvwx
//	go run ./scripts/smoke -free-port     # prints a free TCP port on 127.0.0.1 (for smoke.sh)
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "SMOKE FAILED: %s\n", fmt.Sprintf(format, a...))
	os.Exit(1)
}

func step(msg string) { fmt.Println("==> " + msg) }

func expect(cond bool, format string, a ...any) {
	if !cond {
		fail(format, a...)
	}
}

// reply is a response: status, headers and body.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) text() string { return string(r.body) }

// clip is the first n characters of s, for failure messages.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// client is a browser-ish client: cookies kept, redirects reported, not followed.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *client) do(method, path string, body []byte, hdr map[string]string) reply {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		fail("%s %s: %v", method, path, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		fail("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return reply{res.StatusCode, res.Header, out}
}

func (c *client) get(path string) reply { return c.do("GET", path, nil, nil) }

// postForm posts url-encoded fields.
func (c *client) postForm(path string, fields url.Values) reply {
	return c.do("POST", path, []byte(fields.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

// form builds url.Values from key, value pairs; a value may be a []string.
func form(kv ...any) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		switch x := kv[i+1].(type) {
		case []string:
			v[kv[i].(string)] = x
		default:
			v.Set(kv[i].(string), fmt.Sprint(x))
		}
	}
	return v
}

// receiver is the app's webhook endpoint.
type receiver struct {
	mu     sync.Mutex
	events []delivery
}

type delivery struct {
	header http.Header
	body   []byte
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.events = append(r.events, delivery{req.Header.Clone(), body})
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// find returns the first delivery for which match is true.
func (r *receiver) find(match func(obj map[string]any) bool) *delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.events {
		var j map[string]any
		if json.Unmarshal(r.events[i].body, &j) == nil && match(j) {
			d := r.events[i]
			return &d
		}
	}
	return nil
}

// verifySignature checks a Standard Webhooks signature: the base64 HMAC-SHA256 of
// "id.timestamp.body" under the key inside the whsec_ secret.
func verifySignature(secret string, h http.Header, body []byte) bool {
	raw := strings.TrimPrefix(secret, "whsec_")
	raw += strings.Repeat("=", (4-len(raw)%4)%4) // the secret is unpadded base64
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s.%s.", h.Get("webhook-id"), h.Get("webhook-timestamp"))
	m.Write(body)
	want := base64.StdEncoding.EncodeToString(m.Sum(nil))
	for _, s := range strings.Fields(h.Get("webhook-signature")) {
		if i := strings.LastIndex(s, ","); s[i+1:] == want {
			return true
		}
	}
	return false
}

// waitFor polls fn until it returns something other than nil, or fails after 30s.
func waitFor[T any](what string, fn func() *T) *T {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if v := fn(); v != nil {
			return v
		}
		time.Sleep(300 * time.Millisecond)
	}
	fail("timed out after 30s waiting for %s", what)
	return nil
}

func yes() *bool { t := true; return &t }

func jsonMap(b []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		fail("not JSON: %v\n%s", err, clip(string(b), 300))
	}
	return m
}

// get digs a value out of nested maps and slices: get(m, "attempts", 0, "status").
func get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				return nil
			}
			v = s[k]
		}
	}
	return v
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) int64 { f, _ := v.(float64); return int64(f) }

func main() {
	base := flag.String("base", "", "base URL of the running Paymux")
	code := flag.String("code", "", "the first-run setup code from the server log")
	freePort := flag.Bool("free-port", false, "print a free TCP port on 127.0.0.1 and exit")
	flag.Parse()
	if *freePort {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fail("no free port: %v", err)
		}
		fmt.Println(l.Addr().(*net.TCPAddr).Port)
		l.Close()
		return
	}
	if *base == "" || *code == "" {
		fmt.Fprintln(os.Stderr, "usage: smoke -base URL -code SETUP-CODE")
		os.Exit(2)
	}
	c := newClient(*base)

	// The app's webhook receiver.
	rcv := &receiver{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("receiver: %v", err)
	}
	go http.Serve(ln, rcv)
	receiverURL := fmt.Sprintf("http://127.0.0.1:%d/hooks", ln.Addr().(*net.TCPAddr).Port)

	csrfRE := regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	csrfOf := func(page string) string {
		m := csrfRE.FindStringSubmatch(page)
		expect(m != nil, "no csrf field in the page")
		return m[1]
	}
	// dashboardPost posts a dashboard form, taking the csrf token from a page of the same session.
	dashboardPost := func(path string, fields url.Values, page string) (int, http.Header, string) {
		if page == "" {
			page = "/dashboard/apps"
		}
		pg := c.get(page)
		expect(pg.status == 200, "GET %s: %d", page, pg.status)
		fields.Set("csrf", csrfOf(pg.text()))
		out := c.postForm(path, fields)
		expect(out.status == 200 || out.status == 303, "POST %s: %d\n%s", path, out.status, clip(out.text(), 600))
		return out.status, out.header, out.text()
	}

	step("first-run setup")
	r := c.get("/setup")
	expect(r.status == 200, "GET /setup: %d (is this a fresh data directory?)", r.status)
	r = c.postForm("/setup", form("csrf", csrfOf(r.text()), "token", *code, "email", "admin@example.com", "password", "correct horse battery",
		"password2", "correct horse battery", "org_name", "Smoke Org", "app_name", "Smoke Shop", "app_slug", "smoke-shop"))
	expect(r.status == 200 && strings.Contains(r.text(), "Setup complete"), "POST /setup: %d\n%s", r.status, clip(r.text(), 500))
	m := regexp.MustCompile(`(pmx_test_sk_[A-Za-z0-9_\-]+)`).FindStringSubmatch(r.text())
	expect(m != nil, "the setup page did not show a test API key")
	apiKey := m[1]
	r = c.get("/setup")
	expect(r.status == 404, "/setup should close after use, got %d", r.status)

	step("find the app")
	r = c.get("/dashboard/apps")
	m = regexp.MustCompile(`/dashboard/apps/(app_[0-9a-z]+)`).FindStringSubmatch(r.text())
	expect(r.status == 200 && m != nil, "the app is not listed in the dashboard")
	appID := m[1]

	step("GST profile, so that invoices are issued")
	_, hdr, _ := dashboardPost("/dashboard/tax-entities", form("legal_name", "Smoke Shop Private Limited", "gstin", "27AAPFU0939F1ZV",
		"address", "1 Marine Drive, Mumbai 400001", "signatory_name", "A Director"), "/dashboard/tax-entities")
	entity := regexp.MustCompile(`/dashboard/tax-entities/(\S+)`).FindStringSubmatch(hdr.Get("Location"))
	expect(entity != nil, "the tax entity was not created (Location %q)", hdr.Get("Location"))
	dashboardPost("/dashboard/apps/"+appID+"/tax", form("tax_entity_id", entity[1], "issue_invoices", "1", "gst_rate", "18",
		"prices_include_tax", "1", "sac_code", "998315"), "/dashboard/apps/"+appID)

	step("a simulator provider account, linked to the app")
	_, hdr, _ = dashboardPost("/dashboard/providers", form("connector", "simulator", "name", "Simulator", "mode", "test", "app_id", appID,
		"currency", []string{"INR", "USD", "BDT"}, "purpose", []string{"one_time", "subscription"}), "/dashboard/providers/new?connector=simulator")
	expect(strings.Contains(hdr.Get("Location"), "/dashboard/providers/pa_"), "the simulator account was not created (Location %q)", hdr.Get("Location"))

	step("a webhook endpoint for the app")
	_, _, page := dashboardPost("/dashboard/apps/"+appID+"/endpoints", form("url", receiverURL, "mode", "test"), "/dashboard/apps/"+appID)
	m = regexp.MustCompile(`id="revealed-secret">(whsec_[^<]+)<`).FindStringSubmatch(page)
	expect(m != nil, "the endpoint's signing secret was not shown")
	secret := html.UnescapeString(m[1]) // html/template writes + as &#43;

	api := newClient(*base) // a separate client, so no cookies: API key only
	call := func(method, path string, body any, want int) reply {
		hdr := map[string]string{"Authorization": "Bearer " + apiKey}
		var raw []byte
		if body != nil {
			hdr["Content-Type"] = "application/json"
			raw, _ = json.Marshal(body)
		}
		out := api.do(method, path, raw, hdr)
		if want != 0 {
			expect(out.status == want, "%s %s: %d (wanted %d)\n%s", method, path, out.status, want, clip(out.text(), 500))
		}
		return out
	}
	createPayment := func(amount int) (string, *url.URL) {
		out := call("POST", "/v1/payments", map[string]any{
			"amount": amount, "currency": "INR", "order_ref": fmt.Sprintf("smoke-%d", amount), "description": "Smoke test",
			"return_url": "https://shop.example.com/thanks",
			"customer": map[string]any{"external_ref": "smoke-user", "name": "Asha Rao", "email": "asha@example.com",
				"billing": map[string]any{"state_code": "29", "country": "IN"}}}, 201)
		p := jsonMap(out.body)
		u, err := url.Parse(str(p["checkout_url"]))
		expect(err == nil, "checkout_url %v", p["checkout_url"])
		return str(p["id"]), u
	}
	payment := func(id string) map[string]any { return jsonMap(call("GET", "/v1/payments/"+id, nil, 200).body) }

	step("create a payment through the API")
	pid, checkout := createPayment(49900)
	expect(checkout.Path == "/pay/"+pid, "checkout_url %v", checkout)

	step("the pay page sends the customer to the simulator")
	r = c.get(checkout.Path + "?" + checkout.RawQuery)
	loc := r.header.Get("Location")
	m = regexp.MustCompile(`^https?://[^/]+(/sim/[0-9A-Za-z]+\?pa=pa_[0-9a-z]+)$`).FindStringSubmatch(loc)
	expect(r.status == 303 && m != nil, "pay page: %d Location %q\n%s", r.status, loc, clip(r.text(), 400))
	simPage := m[1]
	r = c.get(simPage)
	expect(r.status == 200 && bytes.Contains(r.body, []byte("Succeed")) && bytes.Contains(r.body, []byte("499.00")), "simulator page: %d\n%s", r.status, clip(r.text(), 400))

	step("click Succeed; the return goes through /return and ends at the shop")
	r = c.postForm(simPage, form("action", "succeed"))
	expect(r.status == 303 && strings.HasPrefix(r.header.Get("Location"), "/return/"), "decision: %d %q", r.status, r.header.Get("Location"))
	r = c.get(r.header.Get("Location"))
	back, _ := url.Parse(r.header.Get("Location"))
	expect(r.status == 303 && back != nil && back.Host == "shop.example.com" && strings.Contains(back.RawQuery, "status=succeeded"), "return: %d %q", r.status, r.header.Get("Location"))
	p := payment(pid)
	expect(str(p["status"]) == "succeeded" && num(p["amount_received"]) == 49900, "payment after return: %v", p)
	expect(str(get(p, "attempts", 0, "provider")) == "simulator" && str(get(p, "attempts", 0, "status")) == "captured", "attempts: %v", p["attempts"])

	step("the app's webhook endpoint received a correctly signed payment.succeeded")
	event := func(kind string) func() *delivery {
		return func() *delivery { return rcv.find(func(j map[string]any) bool { return str(j["type"]) == kind }) }
	}
	d := waitFor("payment.succeeded at the webhook receiver", event("payment.succeeded"))
	expect(verifySignature(secret, d.header, d.body), "the payment.succeeded webhook signature does not verify")
	obj := get(jsonMap(d.body), "data", "object")
	expect(str(get(obj, "id")) == pid && str(get(obj, "status")) == "succeeded", "webhook object: %v", obj)

	step("the invoice PDF")
	invs := waitFor("the invoice", func() *[]any {
		l, _ := get(jsonMap(call("GET", "/v1/invoices?payment_id="+pid, nil, 200).body), "data").([]any)
		if len(l) == 0 {
			return nil
		}
		return &l
	})
	expect(len(*invs) == 1 && num(get(*invs, 0, "total")) == 49900, "invoices: %v", *invs)
	pdf := waitFor("the invoice PDF", func() *[]byte {
		out := call("GET", "/v1/invoices/"+str(get(*invs, 0, "id"))+"/pdf", nil, 0)
		if out.status != 200 {
			return nil // rendered by a job, so it may take a moment
		}
		return &out.body
	})
	expect(bytes.HasPrefix(*pdf, []byte("%PDF")) && len(*pdf) > 2000, "not a PDF: %q (%d bytes)", clip(string(*pdf), 20), len(*pdf))

	step("a partial refund")
	out := call("POST", "/v1/payments/"+pid+"/refunds", map[string]any{"amount": 10000, "reason": "smoke test"}, 201)
	rid := str(jsonMap(out.body)["id"])
	ref := waitFor("the refund to succeed", func() *map[string]any {
		j := jsonMap(call("GET", "/v1/refunds/"+rid, nil, 200).body)
		if str(j["status"]) != "succeeded" {
			return nil
		}
		return &j
	})
	expect(strings.HasPrefix(str((*ref)["provider_refund_id"]), "sim_rfnd_"), "refund: %v", *ref)
	d = waitFor("refund.succeeded at the webhook receiver", event("refund.succeeded"))
	expect(verifySignature(secret, d.header, d.body), "the refund.succeeded webhook signature does not verify")
	expect(num(payment(pid)["amount_refunded"]) == 10000, "the payment does not show the refund")
	creditNotes := func(n int) func() *[]any {
		return func() *[]any {
			l, _ := get(jsonMap(call("GET", "/v1/credit_notes?payment_id="+pid, nil, 200).body), "data").([]any)
			if len(l) < n {
				return nil
			}
			return &l
		}
	}
	notes := waitFor("the credit note", creditNotes(1))
	expect(len(*notes) == 1, "credit notes: %v", *notes)

	step("a refund made in the provider's own dashboard is booked as a provider refund")
	r = c.get("/dashboard/payments/" + pid)
	expect(r.status == 200 && bytes.Contains(r.body, []byte("Refund from the provider")), "the payment page has no provider-refund button: %d", r.status)
	pageHTML := r.text()
	att := regexp.MustCompile(`<option value="(att_[0-9a-z]+)"`).FindStringSubmatch(pageHTML)
	expect(att != nil, "the payment page lists no simulator attempt")
	r = c.postForm("/dashboard/payments/"+pid+"/simulate-refund", form("csrf", csrfOf(pageHTML), "attempt_id", att[1], "amount", "50.00"))
	expect(r.status == 200 && bytes.Contains(r.body, []byte("Refund made at the simulated provider")), "simulate-refund: %d\n%s", r.status, clip(r.text(), 400))
	waitFor("the provider refund to be booked", func() *bool {
		if num(payment(pid)["amount_refunded"]) == 15000 {
			return yes()
		}
		return nil
	})
	d = waitFor("refund.succeeded (initiated_by=provider) at the webhook receiver", func() *delivery {
		return rcv.find(func(j map[string]any) bool {
			return str(j["type"]) == "refund.succeeded" && str(get(j, "data", "object", "initiated_by")) == "provider"
		})
	})
	expect(verifySignature(secret, d.header, d.body), "the provider refund webhook signature does not verify")
	obj = get(jsonMap(d.body), "data", "object")
	expect(num(get(obj, "amount")) == 5000 && strings.HasPrefix(str(get(obj, "provider_refund_id")), "sim_rfnd_"), "provider refund object: %v", obj)
	notes = waitFor("the second credit note", creditNotes(2))
	expect(len(*notes) == 2, "credit notes after a provider refund: %v", *notes)

	step("pending, then success by webhook alone")
	pid2, checkout2 := createPayment(25000)
	r = c.get(checkout2.Path + "?" + checkout2.RawQuery)
	m = regexp.MustCompile(`^https?://[^/]+(/sim/\S+)$`).FindStringSubmatch(r.header.Get("Location"))
	expect(m != nil, "the pay page did not redirect to the simulator: %d %q", r.status, r.header.Get("Location"))
	r = c.postForm(m[1], form("action", "pending_succeed", "delay", "2"))
	expect(r.status == 303, "pending decision: %d", r.status)
	c.get(r.header.Get("Location"))
	waitFor("the delayed success to arrive by webhook", func() *bool {
		if str(payment(pid2)["status"]) == "succeeded" {
			return yes()
		}
		return nil
	})

	step("a failed payment")
	pid3, checkout3 := createPayment(1000)
	r = c.get(checkout3.Path + "?" + checkout3.RawQuery)
	m = regexp.MustCompile(`^https?://[^/]+(/sim/\S+)$`).FindStringSubmatch(r.header.Get("Location"))
	expect(m != nil, "the pay page did not redirect to the simulator: %d %q", r.status, r.header.Get("Location"))
	r = c.postForm(m[1], form("action", "fail"))
	c.get(r.header.Get("Location"))
	p3 := waitFor("the failed attempt", func() *map[string]any {
		pp := payment(pid3)
		if str(get(pp, "attempts", 0, "status")) == "failed" {
			return &pp
		}
		return nil
	})
	expect(str((*p3)["status"]) != "succeeded" && str(get(*p3, "attempts", 0, "failure_code")) == "simulated_failure", "failed payment: %v", *p3)

	fmt.Println("SMOKE OK")
}
