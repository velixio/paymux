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
	}{
		"no IP":    {nil, nil, ""},
		"unknown":  {ip("8.8.8.8"), nil, ""},
		"clean":    {ip("8.8.8.8"), map[string]iprep.Info{"8.8.8.8": {Country: "US"}}, ""},
		"relay":    {ip("8.8.8.8"), map[string]iprep.Info{"8.8.8.8": {Relay: true}}, ""},
		"vpn wins": {ip("8.8.8.8", "1.1.1.1"), map[string]iprep.Info{"1.1.1.1": {VPN: true}}, "VPN"},
	} {
		if got := checkoutProxy(tc.ips, tc.infos); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}
