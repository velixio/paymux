package connectors

import (
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/paymux/paymux/connectors/cashfree"
	_ "github.com/paymux/paymux/connectors/payu"
	_ "github.com/paymux/paymux/connectors/razorpay"
	_ "github.com/paymux/paymux/connectors/stripe"
	_ "github.com/paymux/paymux/connectors/whop"
	_ "github.com/paymux/paymux/connectors/zoho"
	"github.com/paymux/paymux/pkg/connector"
)

// Every sandbox signal each connector's config has, one row each: a live
// account carrying it is refused, and the same account without it is not.
func TestCheckCredentialModePerConnector(t *testing.T) {
	cases := []struct {
		name, mode, cfg string
		refused         bool
	}{
		{"cashfree sandbox switch", "live", `{"client_id":"123456","client_secret":"cfsk_ma_prod_x","sandbox":true}`, true},
		{"cashfree test app id", "live", `{"client_id":"TEST10012345","client_secret":"cfsk_ma_prod_x"}`, true},
		{"cashfree test secret", "live", `{"client_id":"123456","client_secret":"cfsk_ma_test_x"}`, true},
		{"cashfree production", "live", `{"client_id":"123456","client_secret":"cfsk_ma_prod_x"}`, false},
		{"cashfree sandbox on a test account", "test", `{"client_id":"TEST1","client_secret":"cfsk_ma_test_x","sandbox":true}`, false},
		{"payu test host", "live", `{"key":"abc123","salt":"s","test_mode":true}`, true},
		{"payu public test key gtKFFx", "live", `{"key":"gtKFFx","salt":"s"}`, true},
		{"payu public test key JBZaLc", "live", `{"key":"JBZaLc","salt":"s"}`, true},
		{"payu production", "live", `{"key":"abc123","salt":"s"}`, false},
		{"razorpay test key", "live", `{"key_id":"rzp_test_abc","key_secret":"s","webhook_secret":"w"}`, true},
		{"razorpay live key", "live", `{"key_id":"rzp_live_abc","key_secret":"s","webhook_secret":"w"}`, false},
		{"razorpay live key on a test account", "test", `{"key_id":"rzp_live_abc","key_secret":"s","webhook_secret":"w"}`, true},
		{"stripe test secret key", "live", `{"secret_key":"sk_test_abc","webhook_secret":"w"}`, true},
		{"stripe test restricted key", "live", `{"secret_key":"rk_test_abc","webhook_secret":"w"}`, true},
		{"stripe live key", "live", `{"secret_key":"sk_live_abc","webhook_secret":"w"}`, false},
		{"stripe live key on a test account", "test", `{"secret_key":"sk_live_abc","webhook_secret":"w"}`, true},
		{"stripe live restricted key on a test account", "test", `{"secret_key":"rk_live_abc","webhook_secret":"w"}`, true},
		{"whop sandbox switch", "live", `{"api_key":"k","company_id":"biz_1","webhook_secret":"w","sandbox":true}`, true},
		{"whop production", "live", `{"api_key":"k","company_id":"biz_1","webhook_secret":"w"}`, false},
		{"zoho sandbox switch", "live", `{"account_id":"1","client_id":"c","client_secret":"s","webhook_secret":"w","sandbox":true}`, true},
		{"zoho sandbox scopes", "live", `{"account_id":"1","client_id":"c","client_secret":"s","webhook_secret":"w","oauth_scopes":"ZohoPaySandbox.payments.CREATE"}`, true},
		{"zoho sandbox checkout url", "live", `{"account_id":"1","client_id":"c","client_secret":"s","webhook_secret":"w","hosted_checkout_url":"https://paymentssandbox.zoho.in/x"}`, true},
		{"zoho production", "live", `{"account_id":"1","client_id":"c","client_secret":"s","webhook_secret":"w","oauth_scopes":"ZohoPay.payments.CREATE","hosted_checkout_url":"https://payments.zoho.in/x"}`, false},
	}
	for _, c := range cases {
		err := checkCredentialMode(c.mode, json.RawMessage(c.cfg))
		if (err != nil) != c.refused {
			t.Errorf("%s: refused = %v, want %v (%v)", c.name, err != nil, c.refused, err)
		}
	}
}

// A connector that gains a sandbox-like setting must not slip past the check:
// every config property of a registered connector that names a sandbox or test
// switch has to make a live account refuse.
func TestEveryConnectorSandboxSwitchIsChecked(t *testing.T) {
	for _, name := range []string{"cashfree", "payu", "razorpay", "stripe", "whop", "zoho"} {
		_, info, ok := connector.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		var sch struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(info.ConfigSchema, &sch); err != nil {
			t.Fatal(err)
		}
		for prop, p := range sch.Properties {
			l := strings.ToLower(prop)
			if p.Type != "boolean" || !(strings.Contains(l, "sandbox") || strings.Contains(l, "test") || strings.Contains(l, "demo")) {
				continue
			}
			if err := checkCredentialMode("live", json.RawMessage(`{"`+prop+`":true}`)); err == nil {
				t.Errorf("%s: a live account with %s=true is not refused", name, prop)
			}
		}
	}
}
