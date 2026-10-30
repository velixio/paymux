package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/dblog"
	"github.com/velixio/paymux/internal/logging"
	"github.com/velixio/paymux/internal/outbound/stdwebhooks"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// What the scan looks for. Secrets are credentials of every kind the app
// handles; PII is what identifies the buyer. None of it may reach any column of
// the logs table, whatever path it took there.
const (
	leakBearer  = "Bearer tok_abcdef1234567890"
	leakJWT     = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.c2lnbmF0dXJlX3ZhbHVl"
	leakLoginPW = "hunter2-secret-pw"
	leakPEM     = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7"

	piiName    = "Asha Rao"
	piiEmail   = "asha@example.com"
	piiPhone   = "9876543210"
	piiAddress = "221B Baker Street"
	piiVPA     = "asha@okbank"
	piiGSTIN   = "29ABCDE1234F1Z5"
	piiPAN     = "ABCDE1234F"
	piiCard    = "4111111111111111"
)

// leakyBody is what a misbehaving provider answers: it echoes credentials and
// the buyer's details back, as real gateways do in error responses.
func leakyBody() string {
	return `{"error":{"code":"BAD_REQUEST","description":"auth failed for key ` + rzpKeySecret + ` and webhook secret ` + rzpWebhookSecret +
		` (` + leakBearer + `) from ` + piiEmail + ` phone ` + piiPhone + ` card ` + piiCard + `",` +
		`"customer":{"name":"` + piiName + `","email":"` + piiEmail + `","contact":"` + piiPhone + `","vpa":"` + piiVPA + `","gstin":"` + piiGSTIN + `","pan":"` + piiPAN + `",` +
		`"address":"` + piiAddress + `"},"token":"` + leakJWT + `","private_key":"-----BEGIN PRIVATE KEY-----\n` + leakPEM + `\n-----END PRIVATE KEY-----"}}`
}

// chaos makes chosen provider calls fail, once each, with a leaky answer.
type chaos struct {
	next http.Handler
	mu   sync.Mutex
	left map[string]int
}

func (c *chaos) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	if strings.HasPrefix(r.URL.Path, "/v1/payments/") && strings.HasSuffix(r.URL.Path, "/refund") {
		key = "refund"
	}
	c.mu.Lock()
	n := c.left[key]
	if n > 0 {
		c.left[key]--
	}
	c.mu.Unlock()
	if n > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "prov_req_"+strings.NewReplacer(" ", "_", "/", "_").Replace(key))
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, leakyBody())
		return
	}
	c.next.ServeHTTP(w, r)
}

// logTable returns every column of every row as one string per row.
func logTable(t *testing.T, r *running) ([]store.LogRow, []string) {
	t.Helper()
	rows := logRows(t, r)
	var flat []string
	for _, x := range rows {
		flat = append(flat, strings.Join([]string{x.ID, x.At.String(), x.Level, x.Kind, x.Component, x.Msg, x.RequestID, x.JobID, x.OrgID, x.AppID,
			x.PaymentID, x.AttemptID, x.RefundID, x.SubscriptionID, x.ProviderAccountID, x.Connector, x.IDs, x.Attrs, x.Request, x.Response,
			fmt.Sprint(x.Status), fmt.Sprint(x.DurationMS)}, "\x1f"))
	}
	return rows, flat
}

// TestNoSecretOrPersonalDataReachesTheLogTable drives real flows (a payment
// through the pay page, a signed and a forged webhook, a refund, rejected API
// keys and logins, a failing hook job) against a provider that fails with
// answers echoing every credential and the buyer's details, then scans every
// column of every stored row.
func TestNoSecretOrPersonalDataReachesTheLogTable(t *testing.T) {
	run := func(t *testing.T, url, dataDir string) {
		rzp := newFakeRazorpay()
		ch := &chaos{next: rzp.handler(), left: map[string]int{"POST /v1/orders": 1, "refund": 1}}
		rzpSrv := httptest.NewServer(ch)
		defer rzpSrv.Close()
		recv := &receiver{}
		appSrv := httptest.NewServer(recv)
		defer appSrv.Close()
		seed := seedFlow(t, url, dataDir, appSrv.URL)
		recv.v, _ = stdwebhooks.NewVerifier(seed.endpointSecret)

		cfg := testConfig(t, url, dataDir)
		cfg.AllowPrivateWebhooks = true
		logs := &syncBuf{}
		var app *App
		app, err := New(cfg, WithLogger(logging.New(logs, -4)),
			WithConnectorHTTP(func(pa store.ProviderAccount, info connector.Info, secretValues []string) *http.Client {
				// The default client of an account, with the provider pointed at the fake.
				_, rec := app.logStore()
				return connector.NewHTTPClient(connector.HTTPOptions{Base: connectortest.RedirectClient(rzpSrv).Transport, Secrets: secretValues,
					Recorder: rec.ProviderCall(dblog.ProviderMeta{Connector: pa.Connector, AccountID: pa.ID, OrgID: pa.OrgID})})
			}))
		if err != nil {
			t.Fatal(err)
		}
		app.OnPaymentSucceeded(func(context.Context, PaymentEvent) error {
			return errors.New("crm rejected " + piiEmail + " with " + rzpKeySecret + " " + leakBearer + " password=" + leakLoginPW)
		})
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
		noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		do := func(method, path string, body io.Reader, hdr map[string]string) (int, http.Header, string) {
			t.Helper()
			req, _ := http.NewRequest(method, base+path, body)
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
		api := func(method, path string, body any) (int, map[string]any) {
			t.Helper()
			var rd io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
			code, _, out := do(method, path, rd, map[string]string{"Authorization": "Bearer " + seed.apiKey, "Content-Type": "application/json", "X-Request-Id": "req-scan-api"})
			var m map[string]any
			_ = json.Unmarshal([]byte(out), &m)
			return code, m
		}

		// A payment with a buyer's details; the first order creation at the provider fails.
		code, pay := api("POST", "/v1/payments", map[string]any{"amount": 49900, "currency": "INR", "order_ref": "ord-scan", "description": "Pro plan",
			"return_url": shopBack, "customer": map[string]any{"external_ref": "u1", "name": piiName, "email": piiEmail, "phone": piiPhone,
				"billing": map[string]any{"state_code": "29", "country": "IN", "line1": piiAddress, "gstin": piiGSTIN}}})
		if code != 201 {
			t.Fatalf("create payment: %d %v", code, pay)
		}
		id := pay["id"].(string)
		checkout, _ := neturl.Parse(pay["checkout_url"].(string))
		clientSecret := checkout.Query().Get("t")
		do("GET", checkout.RequestURI(), nil, map[string]string{"X-Request-Id": "req-scan-pay1"}) // fails at the provider
		_, _, page := do("GET", checkout.RequestURI(), nil, map[string]string{"X-Request-Id": "req-scan-pay2"})
		m := returnRE.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("second pay page attempt did not reach the provider: %s", page)
		}
		paID := m[1]
		orderID, _ := rzp.firstOrder()

		// A forged webhook (401) and a rejected API key (401), each carrying credentials and PII.
		forged := `{"entity":"event","event":"payment.captured","payload":{"customer":{"email":"` + piiEmail + `","name":"` + piiName + `"},"api_key":"` + rzpKeySecret + `"}}`
		if c, _, _ := do("POST", "/hooks/"+paID, strings.NewReader(forged), map[string]string{"X-Razorpay-Signature": strings.Repeat("0", 64),
			"X-Razorpay-Event-Id": "evt_forged_scan", "Content-Type": "application/json", "Authorization": leakBearer}); c != http.StatusUnauthorized {
			t.Fatalf("forged webhook = %d", c)
		}
		if c, _, _ := do("GET", "/v1/payments/"+id, nil, map[string]string{"Authorization": "Bearer pmx_test_sk_" + strings.Repeat("Z", 43)}); c != http.StatusUnauthorized {
			t.Fatalf("bad key = %d", c)
		}
		// A failed dashboard login with a password.
		do("POST", "/dashboard/login", strings.NewReader(neturl.Values{"email": {piiEmail}, "password": {leakLoginPW}}.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})

		// The provider captures; the signed webhook succeeds the payment, and the failing hook runs as a job.
		rzp.capture(orderID)
		payEntity := fmt.Sprintf(`"payment":{"entity":{"id":"pay_123","entity":"payment","amount":49900,"currency":"INR","status":"captured","order_id":%q,"method":"upi","amount_refunded":0,"created_at":%d}}`, orderID, time.Now().Unix())
		body := fmt.Sprintf(`{"entity":"event","account_id":"acc_test","event":"payment.captured","contains":["payment"],"payload":{%s},"created_at":%d}`, payEntity, time.Now().Unix())
		mac := hmac.New(sha256.New, []byte(rzpWebhookSecret))
		mac.Write([]byte(body))
		if c, _, _ := do("POST", "/hooks/"+paID, strings.NewReader(body), map[string]string{"X-Razorpay-Signature": hex.EncodeToString(mac.Sum(nil)),
			"X-Razorpay-Event-Id": "evt_scan_ok", "Content-Type": "application/json"}); c != 200 {
			t.Fatalf("signed webhook = %d", c)
		}
		waitFor(t, "payment to succeed", func() bool { _, p := api("GET", "/v1/payments/"+id, nil); return p["status"] == "succeeded" })

		// A refund; the provider's first answer to it is the leaky failure.
		if c, rf := api("POST", "/v1/payments/"+id+"/refunds", map[string]any{"amount": 10000, "reason": "customer request " + piiEmail}); c != 201 {
			t.Fatalf("refund: %d %v", c, rf)
		}
		waitFor(t, "the failed provider calls to be stored", func() bool {
			rows, _ := logTable(t, r)
			orders, refunds, hook := false, false, false
			for _, x := range rows {
				orders = orders || (x.Kind == "provider" && strings.Contains(x.Msg, "/v1/orders"))
				refunds = refunds || (x.Kind == "provider" && strings.Contains(x.Msg, "/refund"))
				hook = hook || (x.Kind == "job" || strings.Contains(x.Msg, "hook")) && x.Level != ""
			}
			return orders && refunds && hook
		})

		rows, flat := logTable(t, r)
		secrets := []string{rzpKeySecret, rzpWebhookSecret, seed.apiKey, seed.endpointSecret, leakBearer, "tok_abcdef1234567890", leakJWT, leakPEM, leakLoginPW,
			clientSecret, strings.Repeat("Z", 43)}
		pii := []string{piiName, piiEmail, piiPhone, piiAddress, piiVPA, piiGSTIN, piiPAN, piiCard}
		for i, line := range flat {
			for _, s := range append(append([]string{}, secrets...), pii...) {
				if s != "" && strings.Contains(line, s) {
					t.Errorf("row %s leaks %q:\n%s", rows[i].ID, s, strings.ReplaceAll(line, "\x1f", " | "))
				}
			}
		}
		// What should be there is there.
		var provider, httpRows, jobRows, other int
		for _, x := range rows {
			if x.Level != "warn" && x.Level != "error" {
				t.Errorf("a %s row was stored: %+v", x.Level, x)
			}
			switch x.Kind {
			case "provider":
				provider++
				if x.Status == 200 || x.Status == 0 && x.Request == "" {
					t.Errorf("a successful or empty provider row: %+v", x)
				}
				if x.Connector != "razorpay" || x.ProviderAccountID != paID || !strings.Contains(x.IDs, "provider_request_id") {
					t.Errorf("provider row lacks its ids: %+v", x)
				}
				// The call can be traced to the payment, attempt or refund it was made for.
				if strings.Contains(x.Msg, "/v1/orders") && (x.PaymentID != id || !strings.HasPrefix(x.AttemptID, "att_")) {
					t.Errorf("the checkout call lacks its payment and attempt ids: %+v", x)
				}
				if strings.Contains(x.Msg, "/refund") && (x.PaymentID != id || !strings.HasPrefix(x.RefundID, "ref_") || !strings.HasPrefix(x.AttemptID, "att_")) {
					t.Errorf("the refund call lacks its payment, attempt and refund ids: %+v", x)
				}
				if x.Level == "error" && (!strings.Contains(x.Response, "[redacted]") || !strings.Contains(x.Response, "BAD_REQUEST")) {
					t.Errorf("provider response should be kept, redacted: %s", x.Response)
				}
			case "http":
				httpRows++
			case "job":
				jobRows++
			default:
				other++
			}
		}
		if provider < 2 || httpRows < 2 {
			t.Errorf("expected provider and http rows: provider=%d http=%d job=%d other=%d", provider, httpRows, jobRows, other)
		}
		shutdown(t, r)
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		run(t, "sqlite://"+dir+"/paymux.db", dir)
	})
	t.Run("postgres", func(t *testing.T) { run(t, storetest.PostgresURL(t), t.TempDir()) })
}
