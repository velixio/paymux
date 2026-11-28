package dashboard

import (
	"testing"

	"github.com/velixio/paymux/internal/iprep"
	"github.com/velixio/paymux/internal/store"
)

func TestCheckoutProxySumsUpTheCheckoutIPs(t *testing.T) {
	ip := func(s ...string) []store.CheckoutIP {
		var out []store.CheckoutIP
		for _, v := range s {
			out = append(out, store.CheckoutIP{IP: v})
		}
		return out
	}
	for name, tc := range map[string]struct {
		ips   []store.CheckoutIP
		infos map[string]iprep.Info
		want  string
		warn  bool
	}{
		"no IP":      {nil, nil, "", false},
		"private IP": {ip("10.0.0.1"), nil, "", false},
		"pending":    {ip("8.8.8.8"), nil, "Pending", false},
		"clean":      {ip("8.8.8.8"), map[string]iprep.Info{"8.8.8.8": {Country: "US"}}, "No proxy", false},
		"relay":      {ip("8.8.8.8"), map[string]iprep.Info{"8.8.8.8": {Relay: true}}, "Relay", false},
		"vpn wins":   {ip("8.8.8.8", "1.1.1.1"), map[string]iprep.Info{"1.1.1.1": {VPN: true}}, "VPN", true},
	} {
		got, warn := checkoutProxy(tc.ips, tc.infos)
		if got != tc.want || warn != tc.warn {
			t.Errorf("%s: got %q warn %v, want %q warn %v", name, got, warn, tc.want, tc.warn)
		}
	}
}
