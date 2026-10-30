package zoho

import (
	"context"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
)

// The mandate flow is tested in mandate_flow_test.go (Zoho's real endpoints);
// this keeps the older-style event.
func TestMandateWebhook(t *testing.T) {
	c := newConn(t, newFake(""), widgetCfg)
	body := `{"event_id":1003,"event_type":"mandate.inactive","event_object":{"mandate":{"mandate_id":"md_z1","status":"inactive","amount":"748.50","currency":"INR"}}}`
	ns, err := c.ParseWebhook(context.Background(), signedWebhook(testSecret, body, nowUnix, nowUnix))
	if err != nil || len(ns) != 1 {
		t.Fatalf("%v %v", ns, err)
	}
	n := ns[0]
	if n.Kind != connector.NotifyMandate || n.EventID != "1003" || n.Mandate.Status != "revoked" || n.Mandate.ProviderMandateID != "md_z1" || n.Mandate.MaxAmount != 74850 {
		t.Errorf("notification = %+v", n)
	}
}
