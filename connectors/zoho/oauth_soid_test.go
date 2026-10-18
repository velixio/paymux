package zoho

import (
	"encoding/json"
	"net/url"
	"testing"
)

// Zoho's sandbox OAuth "differs from production in two parameters" (its sandbox
// setup guide): the scope prefix ZohoPaySandbox, and soid=zohopaysandbox.<account id>
// on the authorization URL. Going live changes soid to zohopay.<account id>.
func TestAuthorizeURLCarriesTheSoidOfTheServer(t *testing.T) {
	c := oauthConn(t, &accountsFake{}, notConnected)
	for name, tc := range map[string]struct{ cfg, soid string }{
		"sandbox": {`{"account_id":"60000000001","client_id":"cid","client_secret":"s","webhook_secret":"w","widget_api_key":"k","sandbox":true}`, "zohopaysandbox.60000000001"},
		"live":    {`{"account_id":"60000000001","client_id":"cid","client_secret":"s","webhook_secret":"w","widget_api_key":"k"}`, "zohopay.60000000001"},
	} {
		raw, err := c.OAuthAuthorizeURL(json.RawMessage(tc.cfg), "https://pay.example.com/cb", "st", []string{"x"})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(raw)
		if got := u.Query().Get("soid"); got != tc.soid {
			t.Errorf("%s: soid = %q want %q (%s)", name, got, tc.soid, raw)
		}
	}
	// With no account id there is nothing to name.
	raw, _ := c.OAuthAuthorizeURL(json.RawMessage(`{"client_id":"cid","client_secret":"s","webhook_secret":"w","widget_api_key":"k"}`), "https://pay.example.com/cb", "st", []string{"x"})
	if u, _ := url.Parse(raw); u.Query().Has("soid") {
		t.Errorf("soid without an account: %s", raw)
	}
}
