package stripe

import (
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func TestConfigureWebhookCreatesEndpointAndReturnsSecret(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/webhook_endpoints", 200, map[string]any{"data": []any{}, "has_more": false})
	f.reply("POST /v1/webhook_endpoints", 200, map[string]any{
		"id": "we_new", "url": "https://pay.example.com/hooks/pa_1", "secret": "whsec_new",
	})
	c := newConn(t, f)
	got, err := c.ConfigureWebhook(bg, connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created || got.Secret != "whsec_new" {
		t.Fatalf("result = %+v", got)
	}
	calls := f.to("POST /v1/webhook_endpoints")
	if len(calls) != 1 {
		t.Fatalf("create calls = %d", len(calls))
	}
	if calls[0].Form.Get("url") != "https://pay.example.com/hooks/pa_1" ||
		calls[0].Header.Get("Idempotency-Key") != "paymux-webhook:https://pay.example.com/hooks/pa_1" {
		t.Errorf("create = %+v", calls[0])
	}
	for _, event := range stripeWebhookEvents {
		if !containsString(calls[0].Form["enabled_events[]"], event) {
			t.Errorf("missing event %q", event)
		}
	}
}

func TestConfigureWebhookUpdatesMatchingEndpoint(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/webhook_endpoints", 200, map[string]any{"data": []any{
		map[string]any{"id": "we_other", "url": "https://other.example/hook"},
		map[string]any{"id": "we_match", "url": "https://pay.example.com/hooks/pa_1"},
	}})
	f.reply("POST /v1/webhook_endpoints/we_match", 200, map[string]any{"id": "we_match"})
	c := newConn(t, f)
	got, err := c.ConfigureWebhook(bg, connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Created || got.Secret != "" {
		t.Fatalf("result = %+v", got)
	}
	if len(f.to("POST /v1/webhook_endpoints")) != 0 || len(f.to("POST /v1/webhook_endpoints/we_match")) != 1 {
		t.Fatalf("calls = %+v", f.calls)
	}
}

func TestConfigureWebhookRejectsIncompleteCreateResponse(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/webhook_endpoints", 200, map[string]any{"data": []any{}})
	f.reply("POST /v1/webhook_endpoints", 200, map[string]any{"id": "we_new"})
	c := newConn(t, f)
	if _, err := c.ConfigureWebhook(bg, connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"}); err == nil {
		t.Fatal("create response without a secret was accepted")
	}
}

func TestConfigureWebhookWillNotClaimExistingEndpointWithoutSecret(t *testing.T) {
	f := newFake()
	f.reply("GET /v1/webhook_endpoints", 200, map[string]any{"data": []any{
		map[string]any{"id": "we_match", "url": "https://pay.example.com/hooks/pa_1"},
	}})
	raw := []byte(`{"secret_key":"` + testKey + `"}`)
	built, err := New(raw, connectorDepsFor(t, f))
	if err != nil {
		t.Fatal(err)
	}
	_, err = built.(connector.WebhookConfigurator).ConfigureWebhook(bg, connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err == nil || len(f.to("POST /v1/webhook_endpoints/we_match")) != 0 {
		t.Fatalf("existing endpoint without a recoverable secret: err=%v calls=%+v", err, f.calls)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
