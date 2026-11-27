package iprep

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVPNAPILookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/203.0.113.24" || r.URL.Query().Get("key") != "k1" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ip":"203.0.113.24","security":{"vpn":true,"proxy":false,"tor":false,"relay":false},
			"location":{"city":"Ashburn","region":"Virginia","country":"United States","country_code":"us"},
			"network":{"network":"203.0.113.0/24","autonomous_system_number":"AS64500","autonomous_system_organization":"EXAMPLE-HOST"}}`))
	}))
	defer srv.Close()
	v := &VPNAPI{Key: "k1", BaseURL: srv.URL + "/api/", Client: srv.Client()}
	info, err := v.Lookup(context.Background(), "203.0.113.24")
	if err != nil {
		t.Fatal(err)
	}
	want := Info{Country: "US", City: "Ashburn", Region: "Virginia", Org: "EXAMPLE-HOST", VPN: true}
	if info != want || !info.Masked() || info.Kind() != "VPN" {
		t.Fatalf("info %+v", info)
	}
}

func TestVPNAPIErrorsNeverCarryTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Invalid API key"}`))
	}))
	v := &VPNAPI{Key: "secret-key", BaseURL: srv.URL + "/api/", Client: srv.Client()}
	if _, err := v.Lookup(context.Background(), "203.0.113.24"); err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("message answer: %v", err)
	}
	srv.Close() // now the request itself fails
	if _, err := v.Lookup(context.Background(), "203.0.113.24"); err == nil || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("transport error: %v", err)
	}
}

func TestOnlyPublicIPsAreLookedUp(t *testing.T) {
	for ip, want := range map[string]bool{"203.0.113.24": true, "2001:4860::1": true, "10.0.0.1": false, "127.0.0.1": false,
		"192.168.1.5": false, "fe80::1": false, "not-an-ip": false, "::ffff:203.0.113.24": true} {
		if got := Public(ip); got != want {
			t.Errorf("Public(%q) = %v", ip, got)
		}
	}
	if _, err := (&VPNAPI{Key: "k"}).Lookup(context.Background(), "10.0.0.1"); err != ErrNotPublic {
		t.Fatalf("private IP: %v", err)
	}
}

func TestEchoIPLookupAndChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/203.0.113.24/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ip":"203.0.113.24","country":"United States","country_iso":"US","region_name":"New York",
			"city":"New York","asn":"AS64500","asn_org":"Example Hosting"}`))
	}))
	defer srv.Close()
	echo := &EchoIP{BaseURL: srv.URL + "/", Client: srv.Client()}
	want := Info{Country: "US", City: "New York", Region: "New York", Org: "Example Hosting"}
	if info, err := echo.Lookup(context.Background(), "203.0.113.24"); err != nil || info != want {
		t.Fatalf("echoip: %+v %v", info, err)
	}
	if _, err := echo.Lookup(context.Background(), "198.51.100.1"); err == nil {
		t.Fatal("an unknown IP gave an answer")
	}

	down := &VPNAPI{Key: "k", BaseURL: "http://127.0.0.1:1/api/"}
	if info, err := (Chain{down, echo}).Lookup(context.Background(), "203.0.113.24"); err != nil || info != want {
		t.Fatalf("chain fallback: %+v %v", info, err)
	}
	if _, err := (Chain{}).Lookup(context.Background(), "203.0.113.24"); err == nil {
		t.Fatal("an empty chain gave an answer")
	}
}
