package httpapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/checkout"
	"github.com/velixio/paymux/internal/connectors"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/fakeconn"
	"github.com/velixio/paymux/internal/httpapi"
	"github.com/velixio/paymux/internal/jobs"
	"github.com/velixio/paymux/internal/secrets"
	"github.com/velixio/paymux/internal/server"
	"github.com/velixio/paymux/internal/settings"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/internal/store/storetest"
	"github.com/velixio/paymux/pkg/connector"
)

func TestProviderActionCheckoutStaysOnTheMerchantBackend(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
		clock := func() time.Time { return now }
		set := settings.New(db)
		if _, err := core.Bootstrap(ctx, db, set); err != nil {
			t.Fatal(err)
		}
		box, err := secrets.NewBox(bytes.Repeat([]byte{4}, 32))
		if err != nil {
			t.Fatal(err)
		}
		base, _ := url.Parse("https://pay.example.com")
		conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base, Now: clock})
		routing := checkout.NewRouting(nil).WithSettings(set)
		svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: jobs.New(db, jobs.Options{Clock: clock}), Router: routing, Now: clock})
		app, err := svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "Shop", Slug: "shop", DefaultReturnURL: "https://shop.example.com/order"})
		if err != nil {
			t.Fatal(err)
		}
		fake := fakeconn.Register("api-action")
		if _, err := conns.CreateAccount(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, connectors.CreateAccountInput{
			Connector: fake.Name, Name: "Fake", Mode: "test", Config: json.RawMessage(fakeconn.ValidConfig), AppID: app.ID,
		}); err != nil {
			t.Fatal(err)
		}
		route := json.RawMessage(`{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"` + fake.Name + `"}]}}}`)
		if err := set.SetOrg(ctx, audit.System, store.DefaultOrgID, "routing", route); err != nil {
			t.Fatal(err)
		}
		if err := set.SetApp(ctx, audit.System, app.OrgID, app.ID, "checkout_mode", json.RawMessage(`"provider_action"`)); err != nil {
			t.Fatal(err)
		}
		key, _, err := svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, app.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		other, err := svc.CreateApp(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, core.CreateAppInput{Name: "Other", Slug: "other"})
		if err != nil {
			t.Fatal(err)
		}
		otherKey, _, err := svc.CreateAPIKey(ctx, audit.System, core.OrgScope{OrgID: store.DefaultOrgID}, other.ID, "test")
		if err != nil {
			t.Fatal(err)
		}
		flow := checkout.New(checkout.Options{DB: db, Core: svc, Conns: conns, Routing: routing, Now: clock})
		api, err := httpapi.New(httpapi.Options{Core: svc, DB: db, Box: box, Checkout: flow, Settings: set, BaseURL: base, RatePerSecond: -1, Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		httpServer := server.New(server.Options{})
		api.Register(httpServer)
		testServer := httptest.NewServer(httpServer.Handler())
		defer testServer.Close()

		callWithKey := func(authKey, path, idempotency, body string) (int, map[string]any) {
			req, _ := http.NewRequest(http.MethodPost, testServer.URL+path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer "+authKey)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", idempotency)
			res, err := testServer.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			var value map[string]any
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatalf("response %d is not JSON: %s", res.StatusCode, raw)
			}
			return res.StatusCode, value
		}
		call := func(path, idempotency, body string) (int, map[string]any) {
			return callWithKey(key, path, idempotency, body)
		}
		attemptCount := func(paymentID string) int {
			var attempts []store.Attempt
			if err := db.Read(ctx, func(q store.Querier) (err error) {
				attempts, err = store.ListAttempts(ctx, q, paymentID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return len(attempts)
		}
		create := func(order string, withBilling bool) map[string]any {
			customer := ""
			if withBilling {
				customer = `,"customer":{"name":"Asha Rao","billing":{"country":"IN","state_code":"27"}}`
			}
			status, payment := call("/v1/payments", "create-"+order, `{"amount":118,"currency":"INR","order_ref":"`+order+`"`+customer+`}`)
			if status != http.StatusCreated {
				t.Fatalf("create %s: %d %+v", order, status, payment)
			}
			return payment
		}
		setMode := func(mode string) {
			t.Helper()
			if err := set.SetApp(ctx, audit.System, app.OrgID, app.ID, "checkout_mode", json.RawMessage(`"`+mode+`"`)); err != nil {
				t.Fatal(err)
			}
		}
		createUnstarted := func(order string, withBilling bool) map[string]any {
			setMode("hosted")
			payment := create(order, withBilling)
			setMode("provider_action")
			return payment
		}
		status, missingKey := callWithKey(key, "/v1/payments", "", `{"amount":118,"currency":"INR","order_ref":"order-no-key","customer":{"name":"Asha Rao","billing":{"country":"IN","state_code":"27"}}}`)
		missingKeyEnvelope, _ := missingKey["error"].(map[string]any)
		if status != http.StatusBadRequest || missingKeyEnvelope["type"] != "invalid_request" {
			t.Fatalf("missing idempotency key: %d %+v", status, missingKey)
		}

		payment := create("order-1", true)
		if payment["checkout_mode"] != "provider_action" || payment["checkout_url"] != nil {
			t.Fatalf("payment response: %+v", payment)
		}
		paymentID := payment["id"].(string)
		initial, _ := payment["checkout"].(map[string]any)
		provider, _ := initial["action"].(map[string]any)
		if payment["status"] != "created" || initial["kind"] != "checkout" || initial["launcher"] != fake.Name || provider["kind"] != "redirect" || attemptCount(paymentID) != 1 {
			t.Fatalf("initial checkout: payment=%+v attempts=%d", payment, attemptCount(paymentID))
		}
		status, _ = callWithKey(otherKey, "/v1/payments/"+paymentID+"/checkout-action", "other-app", `{}`)
		if status != http.StatusNotFound {
			t.Fatalf("other app checkout status = %d", status)
		}
		status, billingError := call("/v1/payments", "create-order-2-missing", `{"amount":118,"currency":"INR","order_ref":"order-2-missing"}`)
		errEnvelope, _ := billingError["error"].(map[string]any)
		if status != http.StatusUnprocessableEntity || errEnvelope["type"] != "billing_required" {
			t.Fatalf("create missing billing response: %d %+v", status, billingError)
		}
		status, invalidCreateBilling := call("/v1/payments", "create-order-2-invalid", `{"amount":118,"currency":"INR","order_ref":"order-2-invalid","customer":{"name":"Asha Rao","billing":{"country":"IN","state_code":"99"}}}`)
		invalidCreateEnvelope, _ := invalidCreateBilling["error"].(map[string]any)
		if status != http.StatusUnprocessableEntity || invalidCreateEnvelope["type"] != "invalid_billing" {
			t.Fatalf("create invalid billing response: %d %+v", status, invalidCreateBilling)
		}
		missingBilling := createUnstarted("order-2", false)
		status, billingError = call("/v1/payments/"+missingBilling["id"].(string)+"/checkout-action", "checkout-order-2", `{}`)
		errEnvelope, _ = billingError["error"].(map[string]any)
		if status != http.StatusUnprocessableEntity || errEnvelope["type"] != "billing_required" {
			t.Fatalf("missing billing response: %d %+v", status, billingError)
		}
		status, invalidBilling := call("/v1/payments/"+missingBilling["id"].(string)+"/checkout-action", "checkout-order-2-invalid", `{"billing":{"name":"Asha Rao","country":"IN","state_code":"99"}}`)
		invalidEnvelope, _ := invalidBilling["error"].(map[string]any)
		if status != http.StatusUnprocessableEntity || invalidEnvelope["type"] != "invalid_billing" {
			t.Fatalf("invalid billing response: %d %+v", status, invalidBilling)
		}
		status, _ = call("/v1/payments/"+missingBilling["id"].(string)+"/checkout-action", "checkout-order-2-valid", `{"billing":{"name":"Asha Rao","country":"IN","state_code":"27"}}`)
		if status != http.StatusOK || attemptCount(missingBilling["id"].(string)) != 1 {
			t.Fatalf("valid supplied billing status = %d", status)
		}

		status, action := call("/v1/payments/"+paymentID+"/checkout-action", "checkout-order-1", `{}`)
		provider, _ = action["action"].(map[string]any)
		if status != http.StatusOK || action["kind"] != "checkout" || action["launcher"] != fake.Name || provider["kind"] != "redirect" {
			t.Fatalf("checkout action: %d %+v", status, action)
		}
		if _, exists := action["checkout_url"]; exists {
			t.Fatalf("checkout action leaked checkout_url: %+v", action)
		}
		status, replay := call("/v1/payments/"+paymentID+"/checkout-action", "checkout-order-1", `{}`)
		if status != http.StatusOK || replay["payment_id"] != paymentID || attemptCount(paymentID) != 1 {
			t.Fatalf("idempotent replay: %d %+v attempts=%d", status, replay, attemptCount(paymentID))
		}
		status, _ = call("/v1/payments/"+paymentID+"/checkout-action", "checkout-order-1-fresh", `{"fresh":true}`)
		if status != http.StatusOK || attemptCount(paymentID) != 2 {
			t.Fatalf("fresh checkout: %d attempts=%d", status, attemptCount(paymentID))
		}

		terminal := create("order-terminal", true)
		terminalID := terminal["id"].(string)
		status, _ = call("/v1/payments/"+terminalID+"/cancel", "cancel-terminal", ``)
		if status != http.StatusOK {
			t.Fatalf("cancel terminal: %d", status)
		}
		status, complete := call("/v1/payments/"+terminalID+"/checkout-action", "checkout-terminal", `{}`)
		if status != http.StatusOK || complete["kind"] != "complete" || complete["status"] != "canceled" || complete["action"] != nil {
			t.Fatalf("terminal checkout: %d %+v", status, complete)
		}

		processing := create("order-processing", true)
		processingID := processing["id"].(string)
		if err := db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE payments SET status = 'processing' WHERE id = ?", processingID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		status, processingError := call("/v1/payments/"+processingID+"/checkout-action", "checkout-processing", `{"fresh":true}`)
		processingEnvelope, _ := processingError["error"].(map[string]any)
		if status != http.StatusConflict || processingEnvelope["type"] != "state_conflict" || attemptCount(processingID) != 1 {
			t.Fatalf("processing checkout: %d %+v", status, processingError)
		}

		calls := 0
		fake.Locked(func() {
			fake.CheckoutFn = func(req connector.CheckoutRequest) (connector.CheckoutResult, error) {
				calls++
				if calls == 1 {
					return connector.CheckoutResult{}, errors.New("temporary provider failure")
				}
				return connector.CheckoutResult{ProviderOrderID: "order_" + req.ProviderRef,
					NextAction: connector.NextAction{Kind: connector.ActionRedirect, URL: "https://fake.example.com/checkout/" + req.ProviderRef}}, nil
			}
		})
		status, failedCreate := call("/v1/payments", "create-order-retry", `{"amount":118,"currency":"INR","order_ref":"order-retry","customer":{"name":"Asha Rao","billing":{"country":"IN","state_code":"27"}}}`)
		failedEnvelope, _ := failedCreate["error"].(map[string]any)
		retryingID, _ := failedEnvelope["payment_id"].(string)
		if status != http.StatusBadGateway || attemptCount(retryingID) != 1 {
			t.Fatalf("provider failure: %d %+v attempts=%d", status, failedCreate, attemptCount(retryingID))
		}
		status, failedReplay := call("/v1/payments", "create-order-retry", `{"amount":118,"currency":"INR","order_ref":"order-retry","customer":{"name":"Asha Rao","billing":{"country":"IN","state_code":"27"}}}`)
		replayEnvelope, _ := failedReplay["error"].(map[string]any)
		if status != http.StatusBadGateway || replayEnvelope["payment_id"] != retryingID || attemptCount(retryingID) != 1 {
			t.Fatalf("provider failure replay: %d %+v attempts=%d", status, failedReplay, attemptCount(retryingID))
		}
		status, _ = call("/v1/payments/"+retryingID+"/checkout-action", "checkout-retry-failed", `{}`)
		if status != http.StatusOK || attemptCount(retryingID) != 2 {
			t.Fatalf("provider retry: %d attempts=%d", status, attemptCount(retryingID))
		}
		status, _ = call("/v1/payments/"+retryingID+"/checkout-action", "checkout-retry-failed", `{}`)
		if status != http.StatusOK || attemptCount(retryingID) != 2 {
			t.Fatalf("successful retry replay: %d attempts=%d", status, attemptCount(retryingID))
		}

		expired := create("order-expired", true)
		now = now.Add(2 * time.Hour)
		status, complete = call("/v1/payments/"+expired["id"].(string)+"/checkout-action", "checkout-expired", `{}`)
		if status != http.StatusOK || complete["kind"] != "complete" || complete["status"] != "expired" {
			t.Fatalf("expired checkout: %d %+v", status, complete)
		}
	})
}
