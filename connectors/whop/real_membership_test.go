package whop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// testdata/membership_real.json is a membership as the Whop sandbox returns it
// (ids replaced): the period is current_period_start / current_period_end (the
// documentation says renewal_period_*), and the price is the renewal_price
// money object.
func TestRealMembershipShape(t *testing.T) {
	body, err := os.ReadFile("testdata/membership_real.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/memberships/mem_RealRenewal01" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"type":"not_found","message":"Membership not found"}}`))
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c, err := New(testConfig, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.(connector.SubscriptionManager).FetchSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "mem_RealRenewal01"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "active" || s.ProviderRef != "SUBREF01" || s.Amount != 999 || s.Currency != "USD" ||
		s.CurrentPeriodStart != time.Date(2026, 10, 18, 3, 22, 33, 120_000_000, time.UTC).UnixMilli() ||
		s.CurrentPeriodEnd != time.Date(2026, 11, 17, 3, 22, 33, 120_000_000, time.UTC).UnixMilli() {
		t.Errorf("%+v", s)
	}
	if _, err := c.(connector.SubscriptionManager).FetchSubscription(context.Background(), connector.SubscriptionRef{ProviderSubscriptionID: "mem_DoesNotExist"}); err == nil {
		t.Error("an unknown membership must be an error")
	}
}

func TestCanceling(t *testing.T) {
	// Whop's documentation lists "canceling": cancelled at the end of the period,
	// still billing and in use until then.
	if got := membershipStatus("canceling"); got != "active" {
		t.Errorf("canceling -> %s", got)
	}
}
