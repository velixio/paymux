package connector_test

import (
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

func TestValidOrigin(t *testing.T) {
	for o, want := range map[string]bool{
		"https://checkout.example.com":      true,
		"https://*.example.com":             true,
		"https://example.com:8443":          true,
		"http://example.com":                false,
		"https://example.com/path":          false,
		"https://example.com;":              false,
		"https://*.com":                     false,
		"https://*":                         false,
		"'unsafe-inline'":                   false,
		"https://example.com https://x.org": false,
		"":                                  false,
	} {
		if got := connector.ValidOrigin(o); got != want {
			t.Errorf("ValidOrigin(%q) = %v, want %v", o, got, want)
		}
	}
}

func TestNextActionAllowed(t *testing.T) {
	h := connector.CheckoutHosts{
		Script:   []string{"https://sdk.example.com"},
		Form:     []string{"https://pay.example.com:8443"},
		Redirect: []string{"https://*.example.org", "https://exact.example.net"},
	}
	redirect := func(u string) connector.NextAction {
		return connector.NextAction{Kind: connector.ActionRedirect, URL: u}
	}
	form := func(u string) connector.NextAction {
		return connector.NextAction{Kind: connector.ActionFormPost, URL: u, Fields: map[string]string{"a": "b"}}
	}
	sdk := func(u string) connector.NextAction {
		return connector.NextAction{Kind: connector.ActionSDK, ScriptURL: u, Options: map[string]any{"a": 1}}
	}
	for name, tc := range map[string]struct {
		a  connector.NextAction
		ok bool
	}{
		"sdk script listed":            {sdk("https://sdk.example.com/v1/sdk.js"), true},
		"sdk script not listed":        {sdk("https://evil.example.com/sdk.js"), false},
		"sdk host listed for form":     {sdk("https://pay.example.com:8443/x.js"), false},
		"form listed with port":        {form("https://pay.example.com:8443/_payment"), true},
		"form wrong port":              {form("https://pay.example.com/_payment"), false},
		"form not listed":              {form("https://evil.example.com/post"), false},
		"redirect wildcard one label":  {redirect("https://a.example.org/c/1"), true},
		"redirect wildcard two labels": {redirect("https://a.b.example.org/c/1"), true},
		"redirect wildcard apex":       {redirect("https://example.org/c/1"), false},
		"redirect lookalike suffix":    {redirect("https://evilexample.org/c/1"), false},
		"redirect exact":               {redirect("https://exact.example.net/x"), true},
		"redirect host case":           {redirect("https://EXACT.example.net/x"), true},
		"redirect userinfo":            {redirect("https://exact.example.net@evil.example.com/x"), false},
		"redirect userinfo on listed":  {redirect("https://user@exact.example.net/x"), false},
		"redirect http":                {redirect("http://exact.example.net/x"), false},
		"redirect relative":            {redirect("/sim/x"), false},
		"redirect protocol relative":   {redirect("//exact.example.net/x"), false},
		"redirect javascript":          {redirect("javascript:alert(1)"), false},
		"redirect listed for script":   {redirect("https://sdk.example.com/x"), false},
		"unknown kind":                 {connector.NextAction{Kind: "popup", URL: "https://exact.example.net/x"}, false},
	} {
		if err := tc.a.Allowed(h); (err == nil) != tc.ok {
			t.Errorf("%s: Allowed = %v, want ok=%v", name, err, tc.ok)
		}
	}
	if err := redirect("https://exact.example.net/x").Allowed(connector.CheckoutHosts{}); err == nil {
		t.Error("no declared hosts must refuse every off-site URL")
	}
	// An invalid entry in the list never matches, whatever it says.
	bad := connector.CheckoutHosts{Redirect: []string{"https://exact.example.net/x", "*", "http://exact.example.net"}}
	if err := redirect("https://exact.example.net/x").Allowed(bad); err == nil {
		t.Error("a malformed entry must not allow anything")
	}
}

func TestNextActionAllowedSelf(t *testing.T) {
	h := connector.CheckoutHosts{Self: "http://hub.example.com:8080"}
	ok := connector.NextAction{Kind: connector.ActionRedirect, URL: "http://hub.example.com:8080/sim/x?pa=1"}
	if err := ok.Allowed(h); err != nil {
		t.Errorf("the hub's own page: %v", err)
	}
	for _, u := range []string{"http://hub.example.com:9090/sim/x", "https://hub.example.com:8080/sim/x", "http://evil.example.com/sim/x"} {
		if err := (connector.NextAction{Kind: connector.ActionRedirect, URL: u}).Allowed(h); err == nil {
			t.Errorf("%s must be refused", u)
		}
	}
	// Self is for redirects to the hub's page only.
	f := connector.NextAction{Kind: connector.ActionFormPost, URL: "http://hub.example.com:8080/x", Fields: map[string]string{"a": "b"}}
	if err := f.Allowed(h); err == nil {
		t.Error("Self must not allow a form post")
	}
}
