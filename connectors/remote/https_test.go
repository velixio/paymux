package remote

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/paymux/paymux/pkg/connector/connectortest"
)

func TestRemoteRequiresHTTPS(t *testing.T) {
	deps := connectortest.NewDeps(nil, now0)
	deps.HTTP = &http.Client{Transport: refuse{}} // never reaches the network
	try := func(url string, insecure bool) error {
		cfg := fmt.Sprintf(`{"base_url":%q,"secret":%q,"allow_insecure":%t,%s}`, url, testSecret, insecure, testHostsJSON)
		_, err := New([]byte(cfg), deps)
		return err
	}
	isScheme := func(err error) bool { return err != nil && strings.Contains(err.Error(), "base_url must") }

	for _, u := range []string{"http://connector.example.com", "http://10.0.0.5:8090/v1", "http://localhost.evil.example.com", "ftp://x.example.com", "connector.example.com"} {
		if err := try(u, false); !isScheme(err) {
			t.Errorf("%s accepted over plain http: %v", u, err)
		}
	}
	// Past the scheme check these fail only at discovery, because nothing listens.
	for _, u := range []string{"https://connector.example.com", "http://localhost:8090", "http://127.0.0.1:8090", "http://[::1]:8090"} {
		if err := try(u, false); err == nil || isScheme(err) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if err := try("http://connector.example.com", true); err == nil || isScheme(err) {
		t.Errorf("allow_insecure did not lift the check: %v", err)
	}
}

type refuse struct{}

func (refuse) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("no network in this test")
}
