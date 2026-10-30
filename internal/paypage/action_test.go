package paypage_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/velixio/paymux/internal/audit"
	"github.com/velixio/paymux/internal/core"
	"github.com/velixio/paymux/internal/store"
	"github.com/velixio/paymux/pkg/connector"
)

type browserAction struct {
	Kind      string `json:"kind"`
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
	Launcher  string `json:"launcher"`
	Action    struct {
		Kind, URL string
	} `json:"action"`
	Billing *struct {
		StateRequired   bool             `json:"state_required"`
		AddressRequired bool             `json:"address_required"`
		CountryRequired bool             `json:"country_required"`
		Values          map[string]any   `json:"values"`
		States          []map[string]any `json:"states"`
	} `json:"billing"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *env) browserAction(c created, origin, token, body string) (int, http.Header, browserAction) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/pay/"+c.id+"/action", strings.NewReader(body))
	req.Host = "pay.example.com"
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Paymux-Checkout-Token", token)
	res, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out browserAction
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("decode %d %s: %v", res.StatusCode, raw, err)
	}
	return res.StatusCode, res.Header, out
}

func TestBrowserActionReturnsValidatedProviderAction(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/orders/1"})
		status, header, out := e.browserAction(c, "https://shop.example.com", c.secret, `{}`)
		if status != http.StatusOK || out.Kind != "checkout" || out.PaymentID != c.id || out.Action.Kind != "redirect" ||
			!strings.HasPrefix(out.Action.URL, "https://fake.example.com/checkout/") {
			t.Fatalf("%d %+v", status, out)
		}
		if header.Get("Access-Control-Allow-Origin") != "https://shop.example.com" || header.Get("Cache-Control") != "no-store" {
			t.Errorf("browser checkout headers: %v", header)
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id) != 1 {
			t.Error("checkout did not create an attempt")
		}
	})
}

func TestBrowserActionRequiresReturnOriginAndSecret(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/orders/1"})
		if status, _, out := e.browserAction(c, "https://evil.example", c.secret, `{}`); status != http.StatusForbidden || out.Error.Type != "origin_not_allowed" {
			t.Fatalf("wrong origin: %d %+v", status, out)
		}
		if status, _, out := e.browserAction(c, "https://shop.example.com", "wrong", `{}`); status != http.StatusNotFound || out.Error.Type != "not_found" {
			t.Fatalf("wrong secret: %d %+v", status, out)
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id) != 0 {
			t.Error("unauthenticated browser request created an attempt")
		}
	})
}

func TestBrowserActionCollectsRequiredBilling(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "billing_details", json.RawMessage(`"required"`)); err != nil {
			t.Fatal(err)
		}
		c := e.create(core.CreatePaymentInput{Currency: "INR", Amount: 1000, ReturnURL: "https://shop.example.com/order"})
		status, _, out := e.browserAction(c, "https://shop.example.com", c.secret, `{}`)
		if status != http.StatusOK || out.Kind != "billing" || out.Billing == nil || !out.Billing.StateRequired || !out.Billing.AddressRequired || len(out.Billing.States) == 0 {
			t.Fatalf("billing request: %d %+v", status, out)
		}
		body := bytes.NewBufferString(`{"billing":{"name":"Buyer","email":"buyer@example.com","country":"IN","state_code":"27","line1":"1 Main Road","postal_code":"400001"}}`)
		status, _, out = e.browserAction(c, "https://shop.example.com", c.secret, body.String())
		if status != http.StatusOK || out.Kind != "checkout" {
			t.Fatalf("billing submit: %d %+v", status, out)
		}
		p := e.payment(c.id)
		customer, err := e.core.GetCustomer(ctx, p.CustomerID)
		if err != nil || customer.Name != "Buyer" || customer.Billing.PostalCode != "400001" {
			t.Fatalf("customer = %+v err=%v", customer, err)
		}
	})
}

func TestBrowserActionPreflight(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/order"})
		req, _ := http.NewRequest(http.MethodOptions, e.srv.URL+"/pay/"+c.id+"/action", nil)
		req.Host = "pay.example.com"
		req.Header.Set("Origin", "https://shop.example.com")
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNoContent || !strings.Contains(res.Header.Get("Access-Control-Allow-Headers"), "Paymux-Checkout-Token") {
			t.Fatalf("preflight: %d %v", res.StatusCode, res.Header)
		}
	})
}

func TestBrowserActionRejectsAmbiguousJSON(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/order"})
		for _, body := range []string{`{"fresh":false}{}`, `{"unknown":true}`} {
			status, _, out := e.browserAction(c, "https://shop.example.com", c.secret, body)
			if status != http.StatusBadRequest || out.Error.Type != "invalid_request" {
				t.Fatalf("body %q: %d %+v", body, status, out)
			}
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id) != 0 {
			t.Error("invalid request created an attempt")
		}
	})
}

func TestBrowserActionCompletedAndUnknownPayments(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/order"})
		if _, err := e.core.CancelPayment(ctx, e.scope, audit.System, c.id); err != nil {
			t.Fatal(err)
		}
		status, _, out := e.browserAction(c, "https://shop.example.com", c.secret, `{}`)
		if status != http.StatusOK || out.Kind != "complete" || out.Status != "canceled" {
			t.Fatalf("completed payment: %d %+v", status, out)
		}
		status, _, out = e.browserAction(created{id: "pay_missing", secret: "secret"}, "https://shop.example.com", "secret", `{}`)
		if status != http.StatusNotFound || out.Error.Type != "not_found" {
			t.Fatalf("unknown payment: %d %+v", status, out)
		}
	})
}

func TestBrowserActionRejectsInvalidBilling(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		if err := e.set.SetApp(ctx, audit.System, store.DefaultOrgID, "app_1", "billing_details", json.RawMessage(`"required"`)); err != nil {
			t.Fatal(err)
		}
		c := e.create(core.CreatePaymentInput{Currency: "INR", Amount: 1000, ReturnURL: "https://shop.example.com/order"})
		body := `{"billing":{"name":"","country":"IN","state_code":"27","line1":"1 Main Road","postal_code":"bad"}}`
		status, _, out := e.browserAction(c, "https://shop.example.com", c.secret, body)
		if status != http.StatusUnprocessableEntity || !strings.HasPrefix(out.Error.Type, "billing.err.") {
			t.Fatalf("invalid billing: %d %+v", status, out)
		}
		if e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id) != 0 {
			t.Error("invalid billing created an attempt")
		}
	})
}

func TestBrowserActionFreshStartsAnotherAttempt(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		c := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/order"})
		firstStatus, _, first := e.browserAction(c, "https://shop.example.com", c.secret, `{}`)
		freshStatus, _, fresh := e.browserAction(c, "https://shop.example.com", c.secret, `{"fresh":true}`)
		if firstStatus != http.StatusOK || freshStatus != http.StatusOK || first.Action.URL == fresh.Action.URL {
			t.Fatalf("fresh checkout: first=%d %+v fresh=%d %+v", firstStatus, first, freshStatus, fresh)
		}
		if n := e.count("SELECT COUNT(*) FROM payment_attempts WHERE payment_id = ?", c.id); n != 2 {
			t.Fatalf("attempts = %d, want 2", n)
		}
	})
}

func TestBrowserActionMapsRoutingAndProviderFailures(t *testing.T) {
	each(t, func(t *testing.T, e *env) {
		noRoute := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/order"})
		if err := e.db.Write(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE payments SET allowed_connectors = ? WHERE id = ?", `["unconfigured"]`, noRoute.id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		status, _, out := e.browserAction(noRoute, "https://shop.example.com", noRoute.secret, `{}`)
		if status != http.StatusUnprocessableEntity || out.Error.Type != "no_route" {
			t.Fatalf("no route: %d %+v", status, out)
		}

		e.fake.Locked(func() {
			e.fake.CheckoutFn = func(connector.CheckoutRequest) (connector.CheckoutResult, error) {
				return connector.CheckoutResult{}, errors.New("provider down")
			}
		})
		failed := e.create(core.CreatePaymentInput{ReturnURL: "https://shop.example.com/order"})
		status, _, out = e.browserAction(failed, "https://shop.example.com", failed.secret, `{}`)
		if status != http.StatusBadGateway || out.Error.Type != "provider_unavailable" {
			t.Fatalf("provider failure: %d %+v", status, out)
		}
	})
}
