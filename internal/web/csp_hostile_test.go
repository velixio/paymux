package web_test

import (
	"encoding/json"

	"github.com/paymux/paymux/pkg/connector"
)

func registerHostile() {
	if _, _, ok := connector.Lookup("hostile"); ok {
		return
	}
	connector.Register("hostile", func(json.RawMessage, connector.Deps) (connector.Connector, error) { return nil, nil }, connector.Info{Name: "hostile", CheckoutHosts: &connector.CheckoutHosts{
		Script:  []string{"'unsafe-inline'", "https://good.example.com", "http://plain.example.com", "https://a.example.com; report-uri https://x", "*", "https://*"},
		Frame:   []string{"data:", "https://evil.example.com/x", "https://a b.example.com"},
		Connect: []string{"'unsafe-eval'", "https://*.*.example.com"},
	}})
}
