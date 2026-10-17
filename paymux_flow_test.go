package paymux

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/paymux/paymux/connectors/razorpay"
	"github.com/paymux/paymux/internal/audit"
	"github.com/paymux/paymux/internal/connectors"
	"github.com/paymux/paymux/internal/core"
	"github.com/paymux/paymux/internal/invoice"
	"github.com/paymux/paymux/internal/jobs"
	"github.com/paymux/paymux/internal/logging"
	"github.com/paymux/paymux/internal/outbound"
	"github.com/paymux/paymux/internal/outbound/stdwebhooks"
	"github.com/paymux/paymux/internal/secrets"
	"github.com/paymux/paymux/internal/settings"
	"github.com/paymux/paymux/internal/store"
	"github.com/paymux/paymux/internal/store/storetest"
	"github.com/paymux/paymux/migrations"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

const (
	rzpKeyID         = "rzp_test_key"
	rzpKeySecret     = "rzp_test_secret"
	rzpWebhookSecret = "whsec_rzp_known"
)

// fakeRazorpay is a minimal in-memory Razorpay API: orders, payments and
// refunds, with basic auth.
type fakeRazorpay struct {
	mu       sync.Mutex
	orders   map[string]map[string]any
	payments map[string]map[string]any
	refunds  map[string]map[string]any
	idem     map[string]string // refund idempotency header -> refund id
	calls    []string
	idemSeen []string
}

func newFakeRazorpay() *fakeRazorpay {
	return &fakeRazorpay{orders: map[string]map[string]any{}, payments: map[string]map[string]any{},
		refunds: map[string]map[string]any{}, idem: map[string]string{}}
}

func (f *fakeRazorpay) handler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	collection := func(items ...any) map[string]any {
		return map[string]any{"entity": "collection", "count": len(items), "items": items}
	}
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		id := fmt.Sprintf("order_%d", len(f.orders)+1)
		in["id"], in["status"], in["entity"] = id, "created", "order"
		f.orders[id] = in
		reply(w, 200, in)
	})
	mux.HandleFunc("GET /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		o, ok := f.orders[r.PathValue("id")]
		if !ok {
			reply(w, 404, map[string]any{"error": map[string]any{"code": "BAD_REQUEST_ERROR", "description": "not found"}})
			return
		}
		reply(w, 200, o)
	})
	mux.HandleFunc("GET /v1/orders/{id}/payments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var items []any
		for _, p := range f.payments {
			if p["order_id"] == r.PathValue("id") {
				items = append(items, p)
			}
		}
		reply(w, 200, collection(items...))
	})
	mux.HandleFunc("GET /v1/payments/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p, ok := f.payments[r.PathValue("id")]
		if !ok {
			reply(w, 404, map[string]any{"error": map[string]any{"code": "BAD_REQUEST_ERROR", "description": "not found"}})
			return
		}
		reply(w, 200, p)
	})
	mux.HandleFunc("POST /v1/payments/{id}/refund", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Header.Get("X-Refund-Idempotency")
		f.idemSeen = append(f.idemSeen, key)
		if id, ok := f.idem[key]; ok {
			reply(w, 200, f.refunds[id])
			return
		}
		id := fmt.Sprintf("rfnd_%d", len(f.refunds)+1)
		rf := map[string]any{"id": id, "entity": "refund", "payment_id": r.PathValue("id"), "amount": in["amount"], "currency": "INR",
			"receipt": in["receipt"], "status": "pending", "created_at": time.Now().Unix()}
		f.refunds[id], f.idem[key] = rf, id
		reply(w, 200, rf)
	})
	mux.HandleFunc("GET /v1/refunds/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rf, ok := f.refunds[r.PathValue("id")]
		if !ok {
			reply(w, 404, map[string]any{"error": map[string]any{"code": "BAD_REQUEST_ERROR", "description": "not found"}})
			return
		}
		reply(w, 200, rf)
	})
	mux.HandleFunc("GET /v1/payments/{id}/refunds", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var items []any
		for _, rf := range f.refunds {
			if rf["payment_id"] == r.PathValue("id") {
				items = append(items, rf)
			}
		}
		reply(w, 200, collection(items...))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if !ok || u != rzpKeyID || p != rzpKeySecret {
			reply(w, 401, map[string]any{"error": map[string]any{"code": "BAD_REQUEST_ERROR", "description": "Authentication failed"}})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (f *fakeRazorpay) called(pattern string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == pattern {
			n++
		}
	}
	return n
}

// capture marks the order paid by payment pay_123 (captured).
func (f *fakeRazorpay) capture(orderID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[orderID]
	o["status"] = "paid"
	f.payments["pay_123"] = map[string]any{"id": "pay_123", "entity": "payment", "amount": o["amount"], "currency": o["currency"],
		"status": "captured", "order_id": orderID, "method": "upi", "amount_refunded": 0, "fee": 1178, "tax": 180,
		"error_code": nil, "error_description": nil, "created_at": time.Now().Unix()}
}

func (f *fakeRazorpay) processRefunds() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rf := range f.refunds {
		rf["status"] = "processed"
	}
}

func (f *fakeRazorpay) firstOrder() (string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, o := range f.orders {
		return id, o
	}
	return "", nil
}

// receiver is the app's webhook endpoint: it verifies Standard Webhooks
// signatures and records the events.
type receiver struct {
	mu     sync.Mutex
	v      *stdwebhooks.Verifier
	events []map[string]any
	bad    int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.v.Verify(req.Header, body); err != nil {
		r.bad++
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var ev map[string]any
	_ = json.Unmarshal(body, &ev)
	r.events = append(r.events, ev)
	w.WriteHeader(200)
}

func (r *receiver) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e["type"].(string))
	}
	return out
}

func (r *receiver) last(typ string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i]["type"] == typ {
			return r.events[i]
		}
	}
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type flowSeed struct {
	apiKey, endpointSecret, accountID, appID string
}

// seedFlow prepares the database before the app starts: org, app, API key,
// a Razorpay account linked to the app, a tax profile and a webhook endpoint.
func seedFlow(t *testing.T, url, dataDir, endpointURL string) flowSeed {
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
	cfg, _ := json.Marshal(map[string]string{"key_id": rzpKeyID, "key_secret": rzpKeySecret, "webhook_secret": rzpWebhookSecret})
	pa, err := conns.CreateAccount(ctx, audit.System, sc, connectors.CreateAccountInput{Connector: "razorpay", Name: "main", Mode: "test",
		Config: cfg, Currencies: []string{"INR"}, AppID: app.ID})
	if err != nil {
		t.Fatal(err)
	}
	inv := invoice.New(invoice.Options{DB: db, Settings: set})
	te, err := inv.AddTaxEntity(ctx, audit.System, invoice.TaxEntityInput{LegalName: "Voice Studio Pvt Ltd", GSTIN: "27AAPFU0939F1ZV",
		Address: "1 Marine Drive, Mumbai", SignatoryName: "A Director"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.SetAppTaxProfile(ctx, audit.System, invoice.AppTaxInput{AppID: app.ID, TaxEntityID: te.ID, IssueInvoices: true,
		GSTRateBP: 1800, PricesIncludeTax: true}); err != nil {
		t.Fatal(err)
	}
	out := outbound.New(outbound.Options{DB: db, Queue: jobs.New(db, jobs.Options{}), Box: box, AllowPrivate: true})
	_, secret, err := out.CreateEndpoint(ctx, outbound.CreateEndpointInput{OrgID: org, AppID: app.ID, URL: endpointURL})
	if err != nil {
		t.Fatal(err)
	}
	return flowSeed{apiKey: key, endpointSecret: secret, accountID: pa.ID, appID: app.ID}
}

var (
	returnRE = regexp.MustCompile(`return\\?/(pa_[0-9a-z]{26})\\?/([0-9A-Z]{20})`)
	shopBack = "https://shop.example.com/thanks"
)

// TestFullPaymentFlow runs the M3 flow end to end against a fake Razorpay:
// create via API, pay page, return, signed webhook, outbound webhook to the
// app, refund via API, refund webhook, refund.succeeded and a credit note.
func TestFullPaymentFlow(t *testing.T) {
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

		// 1. Create the payment through the API (routing uses the seeded defaults:
		//    INR goes to razorpay).
		code, pay := api("POST", "/v1/payments", map[string]any{"amount": 49900, "currency": "INR", "order_ref": "ord-1",
			"description": "Pro plan", "return_url": shopBack,
			"customer": map[string]any{"external_ref": "u1", "name": "Asha", "email": "asha@example.com", "billing": map[string]any{"state_code": "29", "country": "IN"}}})
		if code != 201 {
			t.Fatalf("create payment: %d %v", code, pay)
		}
		id := pay["id"].(string)
		checkout, _ := neturl.Parse(pay["checkout_url"].(string))
		if checkout.Path != "/pay/"+id || checkout.Query().Get("t") == "" {
			t.Fatalf("checkout_url %v", pay["checkout_url"])
		}

		// 2. Open the pay page: the Razorpay SDK page with an order created at the provider.
		code, _, page := do("GET", checkout.RequestURI(), nil, nil)
		if code != 200 || !strings.Contains(page, "checkout.razorpay.com/v1/checkout.js") || !strings.Contains(page, `"razorpay"`) {
			t.Fatalf("pay page: %d %s", code, page)
		}
		orderID, order := rzp.firstOrder()
		if orderID == "" || order["currency"] != "INR" || order["amount"] != float64(49900) {
			t.Fatalf("provider order %v", order)
		}
		m := returnRE.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no return URL in the checkout options: %s", page)
		}
		paID, ref := m[1], m[2]
		if paID != seed.accountID || order["receipt"] != ref {
			t.Fatalf("account %s/%s receipt %v/%s", paID, seed.accountID, order["receipt"], ref)
		}

		// 3. The customer comes back before anything is captured: nothing changes.
		code, hdr, _ := do("GET", "/return/"+paID+"/"+ref, nil, nil)
		loc, _ := neturl.Parse(hdr.Get("Location"))
		if code != 303 || loc == nil || loc.Host != "shop.example.com" || loc.Query().Get("payment_id") != id || loc.Query().Get("status") == "succeeded" {
			t.Fatalf("early return: %d %q", code, hdr.Get("Location"))
		}

		// 4. A forged webhook claims success before anything was paid. The
		//    connector reports it as unsigned, so it is stored and only
		//    triggers a fetch of the provider's truth, which says "unpaid".
		fetchesBefore := rzp.called("GET /v1/payments/pay_123")
		hook := func(event, extra string, sign bool) int {
			body := fmt.Sprintf(`{"entity":"event","account_id":"acc_test","event":%q,"contains":["payment"],"payload":{%s},"created_at":%d}`,
				event, extra, time.Now().Unix())
			mac := hmac.New(sha256.New, []byte(rzpWebhookSecret))
			mac.Write([]byte(body))
			sig := hex.EncodeToString(mac.Sum(nil))
			if !sign {
				sig = strings.Repeat("0", 64)
			}
			evID := "evt_" + event + "_" + extra[:12]
			if !sign {
				evID = "evt_forged_" + event
			}
			c, _, _ := do("POST", "/hooks/"+paID, strings.NewReader(body), map[string]string{"X-Razorpay-Signature": sig, "X-Razorpay-Event-Id": evID, "Content-Type": "application/json"})
			return c
		}
		payEntity := fmt.Sprintf(`"payment":{"entity":{"id":"pay_123","entity":"payment","amount":49900,"currency":"INR","status":"captured","order_id":%q,"method":"upi","amount_refunded":0,"fee":1178,"tax":180,"created_at":%d}}`, orderID, time.Now().Unix())
		if got := hook("payment.captured", payEntity, false); got != 200 {
			t.Fatalf("forged webhook = %d, want 200 (fetch-only path)", got)
		}
		waitFor(t, "the fetch triggered by the forged webhook", func() bool {
			return rzp.called("GET /v1/payments/pay_123") > fetchesBefore
		})
		time.Sleep(200 * time.Millisecond)
		if _, p := api("GET", "/v1/payments/"+id, nil); p["status"] == "succeeded" {
			t.Fatal("an unsigned webhook changed state")
		}

		// 5. The provider really captures; a signed webhook arrives.
		rzp.capture(orderID)
		if got := hook("payment.captured", payEntity, true); got != 200 {
			t.Fatalf("signed webhook = %d", got)
		}
		waitFor(t, "payment to succeed", func() bool {
			_, p := api("GET", "/v1/payments/"+id, nil)
			return p["status"] == "succeeded"
		})
		if _, p := api("GET", "/v1/payments/"+id, nil); p["amount_received"] != float64(49900) {
			t.Errorf("payment %v", p)
		}
		hook("payment.captured", payEntity, true) // provider retries: deduplicated

		// 6. The app endpoint received a signed payment.succeeded.
		waitFor(t, "payment.succeeded at the app endpoint", func() bool { return recv.last("payment.succeeded") != nil })
		ev := recv.last("payment.succeeded")
		obj := ev["data"].(map[string]any)["object"].(map[string]any)
		if obj["id"] != id || obj["status"] != "succeeded" {
			t.Errorf("event object %v", obj)
		}

		// 7. Returning after success lands on the shop with status=succeeded.
		sig := hmac.New(sha256.New, []byte(rzpKeySecret))
		sig.Write([]byte(orderID + "|pay_123"))
		form := neturl.Values{"razorpay_order_id": {orderID}, "razorpay_payment_id": {"pay_123"}, "razorpay_signature": {hex.EncodeToString(sig.Sum(nil))}}
		code, hdr, _ = do("POST", "/return/"+paID+"/"+ref, strings.NewReader(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		loc, _ = neturl.Parse(hdr.Get("Location"))
		if code != 303 || loc.Query().Get("status") != "succeeded" || loc.Query().Get("payment_id") != id {
			t.Fatalf("return after payment: %d %q", code, hdr.Get("Location"))
		}

		// 8. Refund through the API.
		code, ref1 := api("POST", "/v1/payments/"+id+"/refunds", map[string]any{"amount": 10000, "reason": "customer request"})
		if code != 201 || ref1["status"] != "pending" {
			t.Fatalf("refund: %d %v", code, ref1)
		}
		refundID := ref1["id"].(string)
		waitFor(t, "refund to reach the provider", func() bool { return rzp.called("POST /v1/payments/pay_123/refund") >= 1 })
		rzp.mu.Lock()
		idemKey := rzp.idemSeen[0]
		rzp.mu.Unlock()
		if len(idemKey) != 20 {
			t.Errorf("refund idempotency token %q should be the provider_ref", idemKey)
		}
		if _, rf := api("GET", "/v1/refunds/"+refundID, nil); rf["status"] != "pending" {
			t.Errorf("refund finished too early: %v", rf)
		}

		// 9. The provider processes it and sends the refund webhook.
		rzp.processRefunds()
		rzp.mu.Lock()
		rf1 := rzp.refunds["rfnd_1"]
		rzp.mu.Unlock()
		refundEntity := fmt.Sprintf(`"refund":{"entity":{"id":"rfnd_1","entity":"refund","amount":10000,"currency":"INR","payment_id":"pay_123","receipt":%q,"status":"processed","created_at":%d}}`, rf1["receipt"], time.Now().Unix())
		if got := hook("refund.processed", refundEntity, true); got != 200 {
			t.Fatalf("refund webhook = %d", got)
		}
		waitFor(t, "refund.succeeded at the app endpoint", func() bool { return recv.last("refund.succeeded") != nil })
		if _, rf := api("GET", "/v1/refunds/"+refundID, nil); rf["status"] != "succeeded" {
			t.Errorf("refund %v", rf)
		}
		if _, p := api("GET", "/v1/payments/"+id, nil); p["amount_refunded"] != float64(10000) {
			t.Errorf("payment after refund %v", p)
		}

		// 10. The app has a tax profile, so the successful refund issued a credit note
		//    (and the payment an invoice).
		_, cn := api("GET", "/v1/credit_notes?payment_id="+id, nil)
		if list, _ := cn["data"].([]any); len(list) != 1 {
			t.Errorf("credit notes after refund.succeeded: %v", cn)
		}
		_, invs := api("GET", "/v1/invoices?payment_id="+id, nil)
		if list, _ := invs["data"].([]any); len(list) != 1 {
			t.Errorf("invoices: %v", invs)
		}

		// 11. The app saw the whole story, correctly signed and in order.
		waitFor(t, "all events", func() bool { return len(recv.types()) >= 4 })
		types := recv.types()
		idx := func(typ string) int {
			for i, x := range types {
				if x == typ {
					return i
				}
			}
			return -1
		}
		if !(idx("payment.created") >= 0 && idx("payment.created") < idx("payment.succeeded") && idx("payment.succeeded") < idx("refund.created") &&
			idx("refund.created") < idx("refund.succeeded")) {
			t.Errorf("event order %v", types)
		}
		recv.mu.Lock()
		bad := recv.bad
		recv.mu.Unlock()
		if bad != 0 {
			t.Errorf("%d outbound deliveries failed signature verification", bad)
		}
		// A replayed webhook created no second payment.succeeded.
		n := 0
		for _, x := range types {
			if x == "payment.succeeded" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("payment.succeeded delivered %d times", n)
		}
		shutdown(t, r)
	}
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		run(t, "sqlite://"+dir+"/paymux.db", dir)
	})
	t.Run("postgres", func(t *testing.T) { run(t, storetest.PostgresURL(t), t.TempDir()) })
}
