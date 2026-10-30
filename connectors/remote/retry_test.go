package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
	"github.com/velixio/paymux/pkg/connector/wire"
)

// The host's HTTP client retries a GET after a 5xx. Every attempt must be
// signed afresh (new webhook-id and timestamp): a retry that reuses the first
// attempt's id is refused by the service's replay cache, however innocent.
func TestRetriedInfoCarriesAFreshIDPerAttempt(t *testing.T) {
	f := &fullFake{caps: allCaps}
	h, _ := wire.NewHandler(f, testSecret, func() time.Time { return now0 })

	var mu sync.Mutex
	var ids, stamps []string
	failed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get(wire.HeaderID))
		stamps = append(stamps, r.Header.Get(wire.HeaderTimestamp))
		fail := !failed
		failed = true
		mu.Unlock()
		if fail {
			// The service processed the request, so it now remembers the id, and
			// then the load balancer answered 503: the client retries.
			h.ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	// A real retrying client; the clock moves one second per reading.
	var cmu sync.Mutex
	clock := now0
	deps := connectortest.NewDeps(nil, now0)
	deps.Clock = func() time.Time { cmu.Lock(); defer cmu.Unlock(); clock = clock.Add(time.Second); return clock }
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{MaxRetries: 2, Backoff: time.Millisecond, Timeout: 5 * time.Second})
	cfg, _ := json.Marshal(config{BaseURL: srv.URL + "/v1", Secret: testSecret, AllowInsecure: true, CheckoutHosts: testHosts})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatalf("discovery whose first attempt got a 503: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[0] == ids[1] || stamps[0] == stamps[1] {
		t.Fatalf("the two /info attempts shared their id or timestamp: ids=%v stamps=%v", ids, stamps)
	}
	if !c.Info().Has(connector.CapRefund) {
		t.Errorf("info %+v", c.Info())
	}
}

// With a client that does not retry (here a plain one), the call is still
// signed once and the response still verifies.
func TestUnretriedCallsStillSignAndVerify(t *testing.T) {
	c := serve(t, &fullFake{caps: allCaps}, nil)
	if _, err := c.FetchPayment(t.Context(), connector.PaymentRef{ProviderRef: "R"}); err != nil {
		t.Fatal(err)
	}
}
