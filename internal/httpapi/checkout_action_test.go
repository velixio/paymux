package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
)

func TestProviderActionCheckoutStaysOnTheMerchantBackend(t *testing.T) {
	storetest.EachMigrated(t, func(t *testing.T, db *store.DB) {
		set := settings.New(db)
		if _, err := core.Bootstrap(ctx, db, set); err != nil {
			t.Fatal(err)
		}
		box, err := secrets.NewBox(bytes.Repeat([]byte{4}, 32))
		if err != nil {
			t.Fatal(err)
		}
		base, _ := url.Parse("https://pay.example.com")
		conns := connectors.New(connectors.Options{DB: db, Box: box, BaseURL: base})
		routing := checkout.NewRouting(nil).WithSettings(set)
		svc := core.NewService(core.Options{DB: db, Settings: set, Jobs: jobs.New(db, jobs.Options{}), Router: routing})
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
		flow := checkout.New(checkout.Options{DB: db, Core: svc, Conns: conns, Routing: routing})
		api, err := httpapi.New(httpapi.Options{Core: svc, DB: db, Box: box, Checkout: flow, Settings: set, BaseURL: base, RatePerSecond: -1})
		if err != nil {
			t.Fatal(err)
		}
		httpServer := server.New(server.Options{})
		api.Register(httpServer)
		testServer := httptest.NewServer(httpServer.Handler())
		defer testServer.Close()

		call := func(path, idempotency, body string) (int, map[string]any) {
			req, _ := http.NewRequest(http.MethodPost, testServer.URL+path, bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer "+key)
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

		status, payment := call("/v1/payments", "create-order-1", `{"amount":118,"currency":"INR","order_ref":"order-1","customer":{"name":"Asha Rao","billing":{"country":"IN","state_code":"27"}}}`)
		if status != http.StatusCreated || payment["checkout_mode"] != "provider_action" || payment["checkout_url"] != nil {
			t.Fatalf("payment response: %d %+v", status, payment)
		}
		status, missingBilling := call("/v1/payments", "create-order-2", `{"amount":118,"currency":"INR","order_ref":"order-2"}`)
		if status != http.StatusCreated {
			t.Fatalf("second payment response: %d %+v", status, missingBilling)
		}
		status, billingError := call("/v1/payments/"+missingBilling["id"].(string)+"/checkout-action", "checkout-order-2", `{}`)
		errEnvelope, _ := billingError["error"].(map[string]any)
		if status != http.StatusUnprocessableEntity || errEnvelope["type"] != "billing_required" {
			t.Fatalf("missing billing response: %d %+v", status, billingError)
		}
		status, action := call("/v1/payments/"+payment["id"].(string)+"/checkout-action", "checkout-order-1", `{}`)
		provider, _ := action["action"].(map[string]any)
		if status != http.StatusOK || action["kind"] != "checkout" || action["launcher"] != fake.Name || provider["kind"] != "redirect" {
			t.Fatalf("checkout action: %d %+v", status, action)
		}
		if _, exists := action["checkout_url"]; exists {
			t.Fatalf("checkout action leaked checkout_url: %+v", action)
		}
	})
}
