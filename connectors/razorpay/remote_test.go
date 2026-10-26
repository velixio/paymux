package razorpay

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/paymux/paymux/connectors/remote"
	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
	"github.com/paymux/paymux/pkg/connector/wire"
)

// The Razorpay connector, served by a remote-connector service (the wire
// handler) and used through connectors/remote, passes the same conformance
// kit with the same fixtures as the in-process connector. This is the proof
// that the remote hop is transparent: signatures, typed errors, status
// mapping, return handling and the capability set all survive it.
func TestConformanceThroughRemote(t *testing.T) {
	const secret = "whsec_MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	factory := func(_ json.RawMessage, deps connector.Deps) (connector.Connector, error) {
		inner, err := New(testConfig, deps) // reaches the fake provider through deps.HTTP
		if err != nil {
			return nil, err
		}
		h, err := wire.NewHandler(inner, secret, deps.Now)
		if err != nil {
			return nil, err
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		rdeps := deps
		rdeps.HTTP = srv.Client() // the remote client talks to the service, not to the provider
		return remote.New(json.RawMessage(fmt.Sprintf(`{"base_url":%q,"secret":%q,"allow_insecure":true,"checkout_hosts":{"script":["https://checkout.razorpay.com"]}}`, srv.URL, secret)), rdeps)
	}
	connectortest.Run(t, factory, conformanceFixtures(t))
}
