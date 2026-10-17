//go:build sandbox

package payu

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
	"github.com/paymux/paymux/pkg/connector/connectortest"
)

// Run with: go test -tags sandbox ./connectors/payu -run Sandbox
// Needs PAYU_TEST_KEY and PAYU_TEST_SALT (test.payu.in credentials).
func TestSandboxVerifyUnknownTxn(t *testing.T) {
	key, salt := os.Getenv("PAYU_TEST_KEY"), os.Getenv("PAYU_TEST_SALT")
	if key == "" || salt == "" {
		t.Skip("PAYU_TEST_* not set")
	}
	cfg, _ := json.Marshal(config{Key: key, Salt: salt, TestMode: true})
	deps := connectortest.NewDeps(nil, time.Now())
	deps.HTTP = connector.NewHTTPClient(connector.HTTPOptions{Secrets: []string{salt}})
	c, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.FetchPayment(context.Background(), connector.PaymentRef{ProviderRef: "sbx_does_not_exist"})
	if err == nil && snap.Status != connector.PaymentRequiresAction {
		t.Fatalf("unexpected snapshot %+v", snap)
	}
}
