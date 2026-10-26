package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/paymux/paymux/connectors/remote"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

const secret = "whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

var env = map[string]string{
	"REMOTE_SECRET":     secret,
	"RAZORPAY_CONFIG":   `{"key_id":"rzp_test_k","key_secret":"s","webhook_secret":"w"}`,
	"PAYMUX_BASE_URL":   "https://pay.example.com/",
	"PAYMUX_ACCOUNT_ID": "pa_1",
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The service started from its environment is discovered and used through
// connectors/remote like any in-process connector.
func TestServiceIsDiscoveredThroughTheRemoteConnector(t *testing.T) {
	h, err := build(func(k string) string { return env[k] }, quiet())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	deps := connectortest.NewDeps(nil, time.Now()) // the service signs with the wall clock
	deps.HTTP = srv.Client()
	c, err := remote.New(json.RawMessage(fmt.Sprintf(`{"base_url":%q,"secret":%q,"allow_insecure":true,"checkout_hosts":{"script":["https://checkout.razorpay.com"]}}`, srv.URL, secret)), deps)
	if err != nil {
		t.Fatal(err)
	}
	info := c.Info()
	if info.Name != "remote" || info.DisplayName != "Razorpay" || !info.Has(connector.CapRefund) {
		t.Errorf("info = %+v", info)
	}
	if _, ok := c.(connector.Refunder); !ok {
		t.Error("remote lost the Refunder interface")
	}
	if err := c.VerifyWebhook(context.Background(), connector.WebhookRequest{Body: []byte(`{}`)}); err == nil {
		t.Error("an unsigned Razorpay webhook verified")
	}
}

func TestBuildNeedsItsEnvironment(t *testing.T) {
	for k := range env {
		get := func(key string) string {
			if key == k {
				return ""
			}
			return env[key]
		}
		if _, err := build(get, quiet()); err == nil {
			t.Errorf("built without %s", k)
		}
	}
	bad := map[string]string{"REMOTE_SECRET": "not base64!", "RAZORPAY_CONFIG": env["RAZORPAY_CONFIG"], "PAYMUX_BASE_URL": "x", "PAYMUX_ACCOUNT_ID": "a"}
	if _, err := build(func(k string) string { return bad[k] }, quiet()); err == nil {
		t.Error("accepted a bad secret")
	}
}

func TestURLs(t *testing.T) {
	u := urls{base: "https://pay.example.com", account: "pa_1"}
	if u.ReturnURL("REF 1") != "https://pay.example.com/return/pa_1/REF%201" || u.WebhookURL() != "https://pay.example.com/hooks/pa_1" {
		t.Errorf("%s %s", u.ReturnURL("REF 1"), u.WebhookURL())
	}
}
