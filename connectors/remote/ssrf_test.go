package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// base_url is operator-supplied, so it must not be a way to reach internal
// services: literal addresses are refused when the account is built, names are
// refused at dial time by the guarded client the host builds (every resolved
// address must be public), and redirects are never followed.
func TestRemoteRejectsPrivateBaseURL(t *testing.T) {
	var dialed atomic.Int32
	guarded := func(answer ...string) *http.Client {
		return connector.NewHTTPClient(connector.HTTPOptions{DenyPrivate: true,
			LookupIP: func(_ context.Context, host string) ([]net.IP, error) {
				dialed.Add(1)
				var ips []net.IP
				for _, a := range answer {
					ips = append(ips, net.ParseIP(a))
				}
				return ips, nil
			}})
	}
	build := func(url string, hc *http.Client) error {
		deps := connectortest.NewDeps(nil, now0)
		deps.HTTP = hc
		_, err := New([]byte(fmt.Sprintf(`{"base_url":%q,"secret":%q}`, url, testSecret)), deps)
		return err
	}

	// Literal addresses: refused before any request is made.
	for _, u := range []string{"http://127.0.0.1:8090", "https://10.0.0.5", "https://169.254.169.254/latest", "https://192.168.1.10:8443",
		"https://[::1]:8443", "https://[fe80::1]", "https://[fd00::1]", "https://0.0.0.0", "https://100.64.0.1", "https://localhost"} {
		if err := build(u, guarded("93.184.216.34")); !errors.Is(err, connector.ErrPrivateAddress) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if dialed.Load() != 0 {
		t.Errorf("a literal address was resolved or dialled (%d lookups)", dialed.Load())
	}

	// A name that resolves to a private address: refused at dial time. One bad
	// address among good ones is enough.
	for name, answer := range map[string][]string{
		"private":     {"10.0.0.9"},
		"metadata":    {"169.254.169.254"},
		"loopback":    {"127.0.0.1"},
		"mixed":       {"93.184.216.34", "192.168.0.7"},
		"v4-mapped":   {"::ffff:10.1.2.3"},
		"unspecified": {"0.0.0.0"},
		"ula":         {"fc00::5"},
	} {
		err := build("https://connector.example.com", guarded(answer...))
		if !errors.Is(err, connector.ErrPrivateAddress) || errors.Is(err, connector.ErrTemporary) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// A redirect is not followed, even by a host client that would: the
	// internal target is never contacted.
	var hit atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1) }))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest/meta-data", http.StatusFound)
	}))
	defer redirector.Close()
	deps := connectortest.NewDeps(nil, now0)
	deps.HTTP = &http.Client{} // follows redirects by default
	_, err := New([]byte(fmt.Sprintf(`{"base_url":%q,"secret":%q,"allow_insecure":true}`, redirector.URL, testSecret)), deps)
	if err == nil || hit.Load() != 0 {
		t.Errorf("redirect followed: err=%v internal hits=%d", err, hit.Load())
	}

	// allow_insecure is the explicit way to use a local service.
	if err := build("http://127.0.0.1:1", guarded("93.184.216.34")); !errors.Is(err, connector.ErrPrivateAddress) {
		t.Fatal(err)
	}
	local := httptest.NewServer(http.NotFoundHandler())
	defer local.Close()
	deps.HTTP = local.Client()
	_, err = New([]byte(fmt.Sprintf(`{"base_url":%q,"secret":%q,"allow_insecure":true}`, local.URL, testSecret)), deps)
	if errors.Is(err, connector.ErrPrivateAddress) {
		t.Errorf("allow_insecure did not lift the guard: %v", err)
	}
}
