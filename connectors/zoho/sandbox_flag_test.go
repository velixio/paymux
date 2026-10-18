package zoho

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Zoho's sandbox is a separate host (paymentssandbox.zoho.<dc>, scopes
// ZohoPaySandbox.*) and the widget is told with otherOptions.is_test_mode. The
// live account's credentials are refused there ("Not An Authorized User"),
// which is why this is a setting and not guessed. It follows Zoho's widget
// script and documentation; no sandbox account was available to run it.
func TestSandboxFlagSelectsTheSandboxHostAndWidgetMode(t *testing.T) {
	cfg := json.RawMessage(`{"account_id":"acc1","client_id":"c","client_secret":"s","refresh_token":"r","webhook_secret":"w","widget_api_key":"k","sandbox":true}`)
	c, err := New(cfg, connectortest.NewDeps(nil, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	z := c.(*zoho)
	if z.api.apiBase() != "https://paymentssandbox.zoho.in" || z.api.tokenURL() != "https://accounts.zoho.in/oauth/v2/token" {
		t.Errorf("%s %s", z.api.apiBase(), z.api.tokenURL())
	}
	live, _ := New(widgetCfg, connectortest.NewDeps(nil, time.Unix(nowUnix, 0)))
	if live.(*zoho).api.apiBase() != "https://payments.zoho.in" {
		t.Errorf("%s", live.(*zoho).api.apiBase())
	}
}
