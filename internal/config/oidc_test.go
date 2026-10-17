package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestOIDCIsOffByDefault(t *testing.T) {
	c, err := Load(base())
	if err != nil || c.OIDCEnabled() || c.OIDCDefaultRole != "" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestOIDCConfig(t *testing.T) {
	c, err := Load(base("PAYMUX_OIDC_ISSUER", "https://idp.example.com/realms/main/", "PAYMUX_OIDC_CLIENT_ID", "paymux",
		"PAYMUX_OIDC_CLIENT_SECRET", "s3cret", "PAYMUX_OIDC_ALLOWED_DOMAINS", "Example.com, staff.example.org,,"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.OIDCEnabled() || c.OIDCIssuer != "https://idp.example.com/realms/main" || c.OIDCClientID != "paymux" ||
		c.OIDCClientSecret.Reveal() != "s3cret" || c.OIDCDefaultRole != "viewer" ||
		!reflect.DeepEqual(c.OIDCAllowedDomains, []string{"example.com", "staff.example.org"}) {
		t.Fatalf("%+v", c)
	}
	if s := fmt.Sprintf("%v %+v", c.OIDCClientSecret, c); strings.Contains(s, "s3cret") {
		t.Errorf("the client secret prints itself: %s", s)
	}
	c, err = Load(base("PAYMUX_OIDC_ISSUER", "http://localhost:8081", "PAYMUX_OIDC_CLIENT_ID", "x", "PAYMUX_OIDC_CLIENT_SECRET", "y",
		"PAYMUX_OIDC_DEFAULT_ROLE", "auditor"))
	if err != nil || c.OIDCDefaultRole != "auditor" || len(c.OIDCAllowedDomains) != 0 {
		t.Fatalf("local issuer / custom default role: %+v %v", c, err)
	}
}

func TestOIDCConfigErrors(t *testing.T) {
	good := []string{"PAYMUX_OIDC_ISSUER", "https://idp.example.com", "PAYMUX_OIDC_CLIENT_ID", "id", "PAYMUX_OIDC_CLIENT_SECRET", "sec"}
	with := func(kv ...string) func(string) (string, bool) {
		return base(append(append([]string{}, good...), kv...)...)
	}

	for name, tc := range map[string]struct {
		env  func(string) (string, bool)
		want string
	}{
		"issuer only":     {base("PAYMUX_OIDC_ISSUER", "https://idp.example.com"), "must all be set"},
		"no secret":       {base("PAYMUX_OIDC_ISSUER", "https://idp.example.com", "PAYMUX_OIDC_CLIENT_ID", "id"), "must all be set"},
		"domains only":    {base("PAYMUX_OIDC_ALLOWED_DOMAINS", "example.com"), "must all be set"},
		"plain http":      {with("PAYMUX_OIDC_ISSUER", "http://idp.example.com"), "https"},
		"not a url":       {with("PAYMUX_OIDC_ISSUER", "idp.example.com"), "https"},
		"query in issuer": {with("PAYMUX_OIDC_ISSUER", "https://idp.example.com/?a=b"), "https"},
		"bad domain":      {with("PAYMUX_OIDC_ALLOWED_DOMAINS", "example.com,not a domain"), "not a domain"},
		"wildcard":        {with("PAYMUX_OIDC_ALLOWED_DOMAINS", "*.example.com"), "not a domain"},
		"admin default":   {with("PAYMUX_OIDC_DEFAULT_ROLE", "admin"), "must not be admin"},
	} {
		_, err := Load(tc.env)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
