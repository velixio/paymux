package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/paymux/paymux/connectors/simulator"
	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/migrations"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/outbound/stdwebhooks"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
)

// seedSim prepares a database for the simulator: an app with an API key, a
// test-mode simulator account routed for everything, a tax profile and a
// webhook endpoint.
func seedSim(t *testing.T, url, dataDir, endpointURL string) flowSeed {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, url, dataDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx, migrations.FS); err != nil {
		t.Fatal(err)
	}
	set := settings.New(db)
	org, err := core.Bootstrap(ctx, db, set)
	if err != nil {
		t.Fatal(err)
	}
	sc := core.OrgScope{OrgID: org}
	svc := core.NewService(core.Options{DB: db, Settings: set})
	app, err := svc.CreateApp(ctx, audit.System, sc, core.CreateAppInput{Name: "Voice Studio", Slug: "voicestudio"})
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.CreateAPIKey(ctx, audit.System, sc, app.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := neturl.Parse("http://localhost:8080")
	conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base})
	payKey, _, err := svc.CreateAPIKeyWith(ctx, audit.System, sc, app.ID, "test", core.KeyOptions{Scopes: []string{"payouts:write"}})
	if err != nil {
		t.Fatal(err)
	}
	// payout_delay_seconds 1: a payout stays processing for a second, so that only
	// the simulator's signed payout.processed webhook can finish it in a test.
	pa, err := conns.CreateAccount(ctx, audit.System, sc, connectors.CreateAccountInput{Connector: simulator.Name, Name: "simulator", Mode: "test",
		Config: json.RawMessage(`{"payout_delay_seconds":1}`), AppID: app.ID, Purposes: []string{"one_time", "payout"}})
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"simulator"}]}}}`
	if err := set.SetOrg(ctx, audit.System, org, "routing", json.RawMessage(doc)); err != nil {
		t.Fatal(err)
	}
	inv := invoice.New(invoice.Options{DB: db, Settings: set})
	// The DEMO entity of the live server: Karnataka, prices including 18% GST.
	te, err := inv.AddTaxEntity(ctx, audit.System, invoice.TaxEntityInput{LegalName: "DEMO Private Limited", GSTIN: "29AAACD1234D1Z7",
		Address: "12 MG Road, Bengaluru 560001", SignatoryName: "A Director"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.SetAppTaxProfile(ctx, audit.System, invoice.AppTaxInput{AppID: app.ID, TaxEntityID: te.ID, IssueInvoices: true,
		GSTRateBP: 1800, PricesIncludeTax: true}); err != nil {
		t.Fatal(err)
	}
	out := outbound.New(outbound.Options{DB: db, Queue: jobs.New(db, jobs.Options{}), Box: box, AllowPrivate: true})
	_, secret, err := out.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: org, AppID: app.ID, URL: endpointURL, Mode: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return flowSeed{apiKey: key, endpointSecret: secret, accountID: pa.ID, appID: app.ID, payoutKey: payKey}
}

var simPageRE = regexp.MustCompile(`^http://[^/]+(/sim/[0-9A-Za-z]+\?pa=pa_[0-9a-z]+)$`)

// TestSimulatorEndToEnd runs the whole flow with no provider: API, pay page,
// the hosted simulator page, signed webhooks from the simulator into
// /hooks/{account}, outbound webhooks to the app, an invoice PDF, a refund
// and a credit note.
func TestSimulatorEndToEnd(t *testing.T) {
	run := func(t *testing.T, dbURL, dataDir string) {
		recv := &receiver{}
		appSrv := httptest.NewServer(recv)
		defer appSrv.Close()
		seed := seedSim(t, dbURL, dataDir, appSrv.URL)
		recv.v, _ = stdwebhooks.NewVerifier(seed.endpointSecret)

		// The simulator posts its webhooks to Paymux's own base URL, so the
		// base URL has to be the address Paymux really listens on.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		cfg := testConfig(t, dbURL, dataDir)
		cfg.Listen = addr
		cfg.BaseURL, _ = neturl.Parse("http://" + addr)
		cfg.AllowPrivateWebhooks = true
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
		defer func() {
			if t.Failed() {
				t.Logf("app logs:\n%s", logs.String())
			}
		}()
		base := "http://" + addr
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
			code, _, out := do(method, path, rd, map[string]string{"Authorization": "Bearer " + seed.apiKey, "Content-Type": "application/json"})
			var m map[string]any
			_ = json.Unmarshal([]byte(out), &m)
			return code, m
		}
		getPayment := func(id string) map[string]any { _, p := api("GET", "/v1/payments/"+id, nil); return p }
		newPayment := func(amount int) (id, checkoutPath string) {
			code, pay := api("POST", "/v1/payments", map[string]any{"amount": amount, "currency": "INR", "order_ref": "sim-1", "description": "Pro plan",
				"return_url": "https://shop.example.com/thanks",
				"customer":   map[string]any{"external_ref": "u1", "name": "Asha", "email": "asha@example.com", "billing": map[string]any{"state_code": "29", "country": "IN"}}})
			if code != 201 {
				t.Fatalf("create payment: %d %v", code, pay)
			}
			u, _ := neturl.Parse(pay["checkout_url"].(string))
			return pay["id"].(string), u.RequestURI()
		}
		simPage := func(checkoutPath string) string {
			t.Helper()
			code, hdr, body := do("GET", checkoutPath, nil, nil)
			m := simPageRE.FindStringSubmatch(hdr.Get("Location"))
			if code != 303 || m == nil {
				t.Fatalf("the pay page should send the customer to the simulator: %d %q %s", code, hdr.Get("Location"), body)
			}
			return m[1]
		}

		// 1. A payment, the pay page, and the simulator's page.
		id, checkoutPath := newPayment(49900)
		page := simPage(checkoutPath)
		code, _, html := do("GET", page, nil, nil)
		if code != 200 || !strings.Contains(html, "Succeed") || !strings.Contains(html, "499.00") {
			t.Fatalf("simulator page: %d %s", code, html)
		}
		// Reloading the pay page shows the same simulator checkout.
		if again := simPage(checkoutPath); again != page {
			t.Errorf("a reload made a new checkout: %s vs %s", page, again)
		}

		// 2. The customer clicks Succeed; the return goes through the normal path
		//    and ends at the shop with status=succeeded.
		code, hdr, _ := do("POST", page, strings.NewReader(neturl.Values{"action": {"succeed"}}.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if code != 303 || !strings.HasPrefix(hdr.Get("Location"), "/return/") {
			t.Fatalf("decision: %d %q", code, hdr.Get("Location"))
		}
		code, hdr, _ = do("GET", hdr.Get("Location"), nil, nil)
		loc, _ := neturl.Parse(hdr.Get("Location"))
		if code != 303 || loc == nil || loc.Host != "shop.example.com" || loc.Query().Get("status") != "succeeded" || loc.Query().Get("payment_id") != id {
			t.Fatalf("return: %d %q", code, hdr.Get("Location"))
		}
		if p := getPayment(id); p["status"] != "succeeded" || p["amount_received"] != float64(49900) {
			t.Fatalf("payment %v", p)
		}
		attempts, _ := getPayment(id)["attempts"].([]any)
		if len(attempts) != 1 || attempts[0].(map[string]any)["provider"] != "simulator" || attempts[0].(map[string]any)["status"] != "captured" {
			t.Errorf("attempts %v", attempts)
		}

		// 3. The app's endpoint gets payment.succeeded, the simulator's own signed
		//    webhook was accepted without any error, and an invoice exists.
		waitFor(t, "payment.succeeded at the app", func() bool { return recv.last("payment.succeeded") != nil })
		waitFor(t, "the invoice", func() bool {
			_, l := api("GET", "/v1/invoices?payment_id="+id, nil)
			list, _ := l["data"].([]any)
			return len(list) == 1
		})
		_, l := api("GET", "/v1/invoices?payment_id="+id, nil)
		invObj := l["data"].([]any)[0].(map[string]any)
		invID := invObj["id"].(string)
		// A Karnataka buyer of a Karnataka entity: CGST and SGST, and the invoice
		// is exactly what was received.
		num := func(k string) float64 { v, _ := invObj[k].(float64); return v }
		if num("total") != 49900 || num("igst") != 0 || num("cgst") <= 0 || num("cgst") != num("sgst") || num("taxable")+num("cgst")+num("sgst") != 49900 ||
			invObj["supply_type"] != "b2c" || invObj["place_of_supply"] != "29" {
			t.Errorf("the invoice does not match the payment: %v", invObj)
		}
		var pdf string
		waitFor(t, "the invoice PDF", func() bool { // rendered by a job
			c, _, body := do("GET", "/v1/invoices/"+invID+"/pdf", nil, map[string]string{"Authorization": "Bearer " + seed.apiKey})
			pdf = body
			return c == 200
		})
		if !strings.HasPrefix(pdf, "%PDF") {
			t.Errorf("invoice PDF: %.20q", pdf)
		}
		if strings.Contains(logs.String(), "signature") && strings.Contains(logs.String(), "invalid") {
			t.Errorf("a simulator webhook was rejected:\n%s", logs.String())
		}

		// 4. A refund through the API: the simulator refunds at once and says so by
		//    webhook; the app hears refund.succeeded and a credit note exists.
		code, rf := api("POST", "/v1/payments/"+id+"/refunds", map[string]any{"amount": 10000, "reason": "customer request"})
		if code != 201 {
			t.Fatalf("refund: %d %v", code, rf)
		}
		refundID := rf["id"].(string)
		waitFor(t, "refund.succeeded at the app", func() bool { return recv.last("refund.succeeded") != nil })
		if _, g := api("GET", "/v1/refunds/"+refundID, nil); g["status"] != "succeeded" || !strings.HasPrefix(g["provider_refund_id"].(string), "sim_rfnd_") {
			t.Errorf("refund %v", g)
		}
		if p := getPayment(id); p["amount_refunded"] != float64(10000) {
			t.Errorf("payment after refund: %v", p["amount_refunded"])
		}
		_, cn := api("GET", "/v1/credit_notes?payment_id="+id, nil)
		if list, _ := cn["data"].([]any); len(list) != 1 {
			t.Errorf("credit notes: %v", cn)
		}

		// 5. Pending, then succeed: the customer is back at the shop with the
		//    payment still processing, and only the simulator's signed webhook
		//    (not a return or a poll) moves it to succeeded.
		id2, checkout2 := newPayment(25000)
		page2 := simPage(checkout2)
		code, hdr, _ = do("POST", page2, strings.NewReader(neturl.Values{"action": {"pending_succeed"}, "delay": {"1"}}.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if code != 303 {
			t.Fatalf("pending decision: %d", code)
		}
		code, hdr, _ = do("GET", hdr.Get("Location"), nil, nil)
		loc, _ = neturl.Parse(hdr.Get("Location"))
		if code != 303 || loc.Query().Get("status") == "succeeded" {
			t.Fatalf("pending return: %d %q", code, hdr.Get("Location"))
		}
		waitFor(t, "the delayed success", func() bool { return getPayment(id2)["status"] == "succeeded" })

		// 6. A failure.
		id3, checkout3 := newPayment(1000)
		page3 := simPage(checkout3)
		_, hdr, _ = do("POST", page3, strings.NewReader(neturl.Values{"action": {"fail"}}.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		do("GET", hdr.Get("Location"), nil, nil)
		waitFor(t, "the failed attempt", func() bool {
			a, _ := getPayment(id3)["attempts"].([]any)
			return len(a) == 1 && a[0].(map[string]any)["status"] == "failed" && a[0].(map[string]any)["failure_code"] == "simulated_failure"
		})
		if getPayment(id3)["status"] == "succeeded" {
			t.Error("a failed attempt succeeded the payment")
		}

		// 7. A payout: the simulator keeps it processing for a second and then
		//    tells Paymux by a signed payout.processed webhook, which the normal
		//    inbound pipeline turns into a fetch of the payout and its success.
		payoutAPI := func(method, path string, body any) (int, map[string]any) {
			t.Helper()
			var rd io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				rd = bytes.NewReader(b)
			}
			code, _, out := do(method, path, rd, map[string]string{"Authorization": "Bearer " + seed.payoutKey, "Content-Type": "application/json", "Idempotency-Key": "sim-" + path})
			var m map[string]any
			_ = json.Unmarshal([]byte(out), &m)
			return code, m
		}
		code, po := payoutAPI("POST", "/v1/payouts", map[string]any{"amount": 125000, "currency": "INR", "narration": "October",
			"beneficiary": map[string]any{"name": "Asha Rao", "account_number": "123456789012", "ifsc": "HDFC0001234"}})
		if code != 201 {
			t.Fatalf("payout: %d %v", code, po)
		}
		payoutID := po["id"].(string)
		waitFor(t, "the payout to be paid", func() bool { _, g := payoutAPI("GET", "/v1/payouts/"+payoutID, nil); return g["status"] == "succeeded" })
		if _, g := payoutAPI("GET", "/v1/payouts/"+payoutID, nil); g["amount"] != float64(125000) {
			t.Errorf("payout %v", g)
		}
		if strings.Contains(logs.String(), "payout") && strings.Contains(logs.String(), "not of this") {
			t.Errorf("the payout was refused as not ours:\n%s", logs.String())
		}
		shutdown(t, r)
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		run(t, "sqlite://"+dir+"/paymux.db", dir)
	})
	t.Run("postgres", func(t *testing.T) { run(t, storetest.PostgresURL(t), t.TempDir()) })
}
