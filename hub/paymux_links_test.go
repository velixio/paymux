package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/outbound/stdwebhooks"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// TestPaymentLinkFlow runs a payment link through the whole hub against a fake
// Razorpay: create via the API, open the hosted page, submit the form, pay,
// and see the single-use link switch itself off and the events arrive.
func TestPaymentLinkFlow(t *testing.T) {
	run := func(t *testing.T, url, dataDir string) {
		rzp := newFakeRazorpay()
		rzpSrv := httptest.NewServer(rzp.handler())
		defer rzpSrv.Close()
		recv := &receiver{}
		appSrv := httptest.NewServer(recv)
		defer appSrv.Close()
		seed := seedFlow(t, url, dataDir, appSrv.URL)
		recv.v, _ = stdwebhooks.NewVerifier(seed.endpointSecret)

		cfg := testConfig(t, url, dataDir)
		cfg.AllowPrivateWebhooks = true
		logs := &syncBuf{}
		app, err := New(cfg, WithLogger(logging.New(logs, -4)),
			WithConnectorHTTP(func(store.ProviderAccount, connector.Info, []string) *http.Client {
				return connectortest.RedirectClient(rzpSrv)
			}))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- app.Start(ctx) }()
		r := &running{app: app, logs: logs, done: make(chan struct{}), stop: cancel}
		go func() { r.err = <-done; close(r.done) }()
		waitReady(t, r)
		defer func() {
			if t.Failed() {
				t.Logf("app logs:\n%s", logs.String())
			}
		}()
		base := "http://" + app.Addr()
		jar, _ := cookiejar.New(nil)
		browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		do := func(c *http.Client, method, path string, body io.Reader, hdr map[string]string) (int, http.Header, string) {
			t.Helper()
			req, _ := http.NewRequest(method, base+path, body)
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			return res.StatusCode, res.Header, string(b)
		}
		api := func(method, path string, body any) (int, map[string]any) {
			t.Helper()
			var rd io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
			code, _, out := do(http.DefaultClient, method, path, rd, map[string]string{"Authorization": "Bearer " + seed.apiKey, "Content-Type": "application/json"})
			var m map[string]any
			_ = json.Unmarshal([]byte(out), &m)
			return code, m
		}

		// 1. Create a single-use link through the API.
		code, link := api("POST", "/v1/payment_links", map[string]any{"amount": 49900, "currency": "INR", "description": "Pro plan, annual",
			"single_use": true, "return_url": shopBack, "metadata": map[string]string{"sku": "pro-annual"}})
		if code != 201 {
			t.Fatalf("create link: %d %v", code, link)
		}
		id, slug := link["id"].(string), link["slug"].(string)
		if link["url"] != "http://localhost:8080/l/"+slug {
			t.Fatalf("url %v", link["url"])
		}
		waitFor(t, "payment_link.created at the app endpoint", func() bool { return recv.last("payment_link.created") != nil })
		if obj := recv.last("payment_link.created")["data"].(map[string]any)["object"].(map[string]any); obj["id"] != id || obj["url"] != link["url"] {
			t.Errorf("created event %v", obj)
		}

		// 2. The customer opens the page and submits the form.
		code, _, page := do(browser, "GET", "/l/"+slug, nil, nil)
		csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
		if code != 200 || csrf == nil || !strings.Contains(page, "Pro plan, annual") || !strings.Contains(page, "INR 499.00") || !strings.Contains(page, "/theme/default/theme.css") {
			t.Fatalf("link page: %d %s", code, page)
		}
		form := neturl.Values{"csrf": {csrf[1]}, "name": {"Asha K"}, "email": {"asha@example.com"}, "phone": {"98765 43210"}}
		code, hdr, out := do(browser, "POST", "/l/"+slug, strings.NewReader(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		loc, _ := neturl.Parse(hdr.Get("Location"))
		if code != 303 || loc == nil || !strings.HasPrefix(loc.Path, "/pay/pay_") {
			t.Fatalf("submit: %d %q %s", code, hdr.Get("Location"), out)
		}
		payID := strings.TrimPrefix(loc.Path, "/pay/")

		// 3. The normal pay flow takes over: the INR payment needs a state for the
		//    invoice, then goes to Razorpay.
		code, _, page = do(browser, "GET", loc.RequestURI(), nil, nil)
		csrf = regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page)
		if code != 200 || csrf == nil || !strings.Contains(page, `name="state_code"`) {
			t.Fatalf("pay page should ask for the state first: %d %s", code, page)
		}
		billing := neturl.Values{"csrf": {csrf[1]}, "name": {"Asha K"}, "email": {"asha@example.com"}, "state_code": {"29"}}
		code, hdr, _ = do(browser, "POST", loc.RequestURI(), strings.NewReader(billing.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if code != 303 {
			t.Fatalf("billing: %d", code)
		}
		code, _, page = do(browser, "GET", hdr.Get("Location"), nil, nil)
		if code != 200 || !strings.Contains(page, "checkout.razorpay.com/v1/checkout.js") {
			t.Fatalf("checkout: %d %s", code, page)
		}
		orderID, order := rzp.firstOrder()
		if orderID == "" || order["amount"] != float64(49900) {
			t.Fatalf("provider order %v", order)
		}
		if code, p := api("GET", "/v1/payments/"+payID, nil); code != 200 || p["metadata"].(map[string]any)["payment_link_id"] != id || p["metadata"].(map[string]any)["sku"] != "pro-annual" {
			t.Fatalf("payment %v", p)
		}

		// 4. While the payment is open, a second visitor is turned away.
		jar2, _ := cookiejar.New(nil)
		other := &http.Client{Jar: jar2, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		_, _, page2 := do(other, "GET", "/l/"+slug, nil, nil)
		csrf2 := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(page2)
		form2 := neturl.Values{"csrf": {csrf2[1]}, "name": {"Ravi"}, "email": {"ravi@example.com"}}
		if code, _, body := do(other, "POST", "/l/"+slug, strings.NewReader(form2.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); code != 409 || !strings.Contains(body, "being used right now") {
			t.Fatalf("second visitor: %d %s", code, body)
		}

		// 5. The provider captures; the signed webhook arrives; the payment succeeds.
		rzp.capture(orderID)
		payEntity := fmt.Sprintf(`"payment":{"entity":{"id":"pay_123","entity":"payment","amount":49900,"currency":"INR","status":"captured","order_id":%q,"method":"upi","amount_refunded":0,"fee":1178,"tax":180,"created_at":%d}}`, orderID, time.Now().Unix())
		body := fmt.Sprintf(`{"entity":"event","account_id":"acc_test","event":"payment.captured","contains":["payment"],"payload":{%s},"created_at":%d}`, payEntity, time.Now().Unix())
		mac := hmac.New(sha256.New, []byte(rzpWebhookSecret))
		mac.Write([]byte(body))
		code, _, _ = do(http.DefaultClient, "POST", "/hooks/"+seed.accountID, strings.NewReader(body), map[string]string{
			"X-Razorpay-Signature": hex.EncodeToString(mac.Sum(nil)), "X-Razorpay-Event-Id": "evt_link_1", "Content-Type": "application/json"})
		if code != 200 {
			t.Fatalf("webhook: %d", code)
		}
		waitFor(t, "payment to succeed", func() bool { _, p := api("GET", "/v1/payments/"+payID, nil); return p["status"] == "succeeded" })

		// 6. The single-use link is spent, in the same transaction, and said so.
		waitFor(t, "payment_link.deactivated at the app endpoint", func() bool { return recv.last("payment_link.deactivated") != nil })
		obj := recv.last("payment_link.deactivated")["data"].(map[string]any)["object"].(map[string]any)
		if obj["id"] != id || obj["active"] != false || obj["deactivated_reason"] != "used" || obj["payment_id"] != payID {
			t.Errorf("deactivated event %v", obj)
		}
		if _, l := api("GET", "/v1/payment_links/"+id, nil); l["active"] != false || l["deactivated_reason"] != "used" {
			t.Errorf("link %v", l)
		}
		if code, _, body := do(browser, "GET", "/l/"+slug, nil, nil); code != 410 || !strings.Contains(body, "no longer active") {
			t.Errorf("spent link page: %d %s", code, body)
		}
		if n := countOf(recv.types(), "payment_link.deactivated"); n != 1 {
			t.Errorf("%d deactivation events", n)
		}
		shutdown(t, r)
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		run(t, "sqlite://"+dir+"/paymux.db", dir)
	})
	t.Run("postgres", func(t *testing.T) { run(t, storetest.PostgresURL(t), t.TempDir()) })
}

func countOf(all []string, want string) (n int) {
	for _, s := range all {
		if s == want {
			n++
		}
	}
	return
}
