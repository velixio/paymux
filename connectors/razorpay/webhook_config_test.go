package razorpay

import (
	"context"
	"encoding/hex"
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

func webhookConn(t *testing.T, raw json.RawMessage, handler http.Handler) (*razorpay, *[]webhookCall) {
	t.Helper()
	var calls []webhookCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if b, _ := io.ReadAll(req.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		calls = append(calls, webhookCall{Method: req.Method, Path: req.URL.Path, Query: req.URL.Query(), Body: body})
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	c, err := New(raw, connectortest.NewDeps(srv, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	return c.(*razorpay), &calls
}

func TestConfigureWebhookCreatesWithGeneratedSecret(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method + " " + req.URL.Path {
		case "GET /v2/accounts/acc_test123/webhooks":
			writeJSON(w, 200, map[string]any{"entity": "collection", "count": 0, "items": []any{}})
		case "POST /v2/accounts/acc_test123/webhooks":
			writeJSON(w, 200, map[string]any{"id": "hook_1", "url": "https://pay.example.com/hooks/pa_1", "secret_exists": true})
		default:
			writeAPIError(w, 404, "nope")
		}
	})
	raw := json.RawMessage(`{"key_id":"` + testKeyID + `","key_secret":"` + testKeySecret + `","account_id":"acc_test123"}`)
	c, calls := webhookConn(t, raw, h)
	got, err := c.ConfigureWebhook(context.Background(), connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err != nil {
		t.Fatal(err)
	}
	secretBytes, decodeErr := hex.DecodeString(got.Secret)
	if !got.Created || decodeErr != nil || len(secretBytes) != 32 {
		t.Fatalf("result = %+v decode=%v bytes=%d", got, decodeErr, len(secretBytes))
	}
	if len(*calls) != 2 || (*calls)[0].Query.Get("count") != "100" || (*calls)[0].Query.Get("skip") != "0" {
		t.Fatalf("calls = %+v", *calls)
	}
	body := (*calls)[1].Body
	if body["url"] != "https://pay.example.com/hooks/pa_1" || body["secret"] != got.Secret {
		t.Fatalf("create body = %#v", body)
	}
	if events, _ := body["events"].([]any); len(events) != len(razorpayWebhookEvents) {
		t.Fatalf("events = %#v", events)
	}
}

func TestConfigureWebhookUpdatesExistingEndpoint(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method + " " + req.URL.Path {
		case "GET /v2/accounts/acc_test123/webhooks":
			writeJSON(w, 200, map[string]any{"count": 1, "items": []any{map[string]any{"id": "hook_1", "url": "https://pay.example.com/hooks/pa_1", "secret_exists": true}}})
		case "PATCH /v2/accounts/acc_test123/webhooks/hook_1":
			writeJSON(w, 200, map[string]any{"id": "hook_1"})
		default:
			writeAPIError(w, 404, "nope")
		}
	})
	raw := json.RawMessage(`{"key_id":"` + testKeyID + `","key_secret":"` + testKeySecret + `","account_id":"acc_test123","webhook_secret":"` + testWebhookSecret + `"}`)
	c, calls := webhookConn(t, raw, h)
	got, err := c.ConfigureWebhook(context.Background(), connector.ProviderWebhookConfig{URL: "https://pay.example.com/hooks/pa_1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Created || got.Secret != "" || len(*calls) != 2 || (*calls)[1].Method != http.MethodPatch {
		t.Fatalf("result=%+v calls=%+v", got, *calls)
	}
	if _, sent := (*calls)[1].Body["secret"]; sent {
		t.Fatalf("update unexpectedly rotated secret: %#v", (*calls)[1].Body)
	}
}

func TestConfigureWebhookRequiresAccountIDAndProtectsExistingSecret(t *testing.T) {
	c, _ := webhookConn(t, json.RawMessage(`{"key_id":"`+testKeyID+`","key_secret":"`+testKeySecret+`"}`), http.NotFoundHandler())
	if _, err := c.ConfigureWebhook(context.Background(), connector.ProviderWebhookConfig{URL: "https://pay.example/hook"}); err == nil || !strings.Contains(err.Error(), "account_id") {
		t.Fatalf("missing account id: %v", err)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, 200, map[string]any{"count": 1, "items": []any{map[string]any{"id": "hook_1", "url": "https://pay.example/hook", "secret_exists": true}}})
	})
	raw := json.RawMessage(`{"key_id":"` + testKeyID + `","key_secret":"` + testKeySecret + `","account_id":"acc_test123"}`)
	c, calls := webhookConn(t, raw, h)
	if _, err := c.ConfigureWebhook(context.Background(), connector.ProviderWebhookConfig{URL: "https://pay.example/hook"}); err == nil || !strings.Contains(err.Error(), "not stored") || len(*calls) != 1 {
		t.Fatalf("existing secret: err=%v calls=%+v", err, *calls)
	}
}
