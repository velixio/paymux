package whop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

type webhookCall struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

func webhookConnector(t *testing.T, raw json.RawMessage, handler http.Handler) (*whop, *[]webhookCall) {
	t.Helper()
	var calls []webhookCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		calls = append(calls, webhookCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body})
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(raw, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	return c.(*whop), &calls
}

func TestConfigureWebhookCreatesEndpointAndReturnsSecret(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/webhooks":
			write(w, 200, map[string]any{"data": []any{}, "page_info": map[string]any{"has_next_page": false}})
		case "POST /api/v1/webhooks":
			write(w, 200, map[string]any{"id": "hook_new", "url": "https://pay.example.com/hooks/pa_1", "webhook_secret": "whsec_new"})
		default:
			write(w, 404, map[string]any{"error": map[string]any{"message": "nope"}})
		}
	})
	raw := json.RawMessage(`{"api_key":"` + testAPIKey + `","company_id":"biz_1"}`)
	c, calls := webhookConnector(t, raw, h)
	got, err := c.ConfigureWebhook(context.Background(), connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Created || got.Secret != "whsec_new" {
		t.Fatalf("result = %+v", got)
	}
	if len(*calls) != 2 || (*calls)[0].Query.Get("account_id") != "biz_1" {
		t.Fatalf("calls = %+v", *calls)
	}
	body := (*calls)[1].Body
	if body["url"] != "https://pay.example.com/hooks/pa_1" || body["resource_id"] != "biz_1" || body["enabled"] != true {
		t.Fatalf("create body = %#v", body)
	}
	events, _ := body["events"].([]any)
	if len(events) != len(whopWebhookEvents) {
		t.Fatalf("events = %#v", events)
	}
}

func TestConfigureWebhookUpdatesMatchingEndpoint(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/webhooks":
			write(w, 200, map[string]any{"data": []any{map[string]any{"id": "hook_1", "url": "https://pay.example.com/hooks/pa_1"}}, "page_info": map[string]any{}})
		case "PATCH /api/v1/webhooks/hook_1":
			write(w, 200, map[string]any{"id": "hook_1"})
		default:
			write(w, 404, map[string]any{"error": map[string]any{"message": "nope"}})
		}
	})
	c, calls := webhookConnector(t, testConfig, h)
	got, err := c.ConfigureWebhook(context.Background(), connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Created || got.Secret != "" || len(*calls) != 2 || (*calls)[1].Method != http.MethodPatch {
		t.Fatalf("result=%+v calls=%+v", got, *calls)
	}
	if _, sent := (*calls)[1].Body["resource_id"]; sent {
		t.Fatalf("update tried to change resource: %#v", (*calls)[1].Body)
	}
}

func TestConfigureWebhookWillNotLoseExistingSecret(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"data": []any{map[string]any{"id": "hook_1", "url": "https://pay.example.com/hooks/pa_1"}}, "page_info": map[string]any{}})
	})
	raw := json.RawMessage(`{"api_key":"` + testAPIKey + `","company_id":"biz_1"}`)
	c, calls := webhookConnector(t, raw, h)
	_, err := c.ConfigureWebhook(context.Background(), connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err == nil || !strings.Contains(err.Error(), "will not reveal") || len(*calls) != 1 {
		t.Fatalf("err=%v calls=%+v", err, *calls)
	}
}
