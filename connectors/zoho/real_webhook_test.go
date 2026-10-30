package zoho

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

var connectortestCheck = connectortest.CheckNotifications

// signedWebhookMS signs like Zoho: HMAC-SHA256 of "t.body" with t in milliseconds.
func signedWebhookMS(secret, body string, tsMS, receivedMS int64) connector.WebhookRequest {
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.%s", tsMS, body)
	return connector.WebhookRequest{
		Method: "POST", Body: []byte(body), ReceivedAt: receivedMS,
		Headers: map[string][]string{"X-Zoho-Webhook-Signature": {fmt.Sprintf("t=%d,v=%s", tsMS, hex.EncodeToString(m.Sum(nil)))}},
	}
}

// Zoho's events are an envelope {event_id (a number), event_type, account_id,
// live_mode, event_time (milliseconds), event_object: {payment|refund|mandate}},
// and the signature header's t is in milliseconds too (documented examples:
// "1734340423138.{...}"). The first version of the connector expected a
// "payload" key, a string event id and seconds, so it saw no event at all.

func TestRealWebhookEnvelope(t *testing.T) {
	c := newRFake().conn(t, widgetCfg)
	ctx := context.Background()
	body := string(raw(t, "event_payment_succeeded"))
	req := signedWebhookMS(testSecret, body, 1792283901000, 1792283902000)
	if err := c.VerifyWebhook(ctx, req); err != nil {
		t.Fatalf("a millisecond timestamp must verify: %v", err)
	}
	ns, err := c.ParseWebhook(ctx, req)
	if err != nil || len(ns) != 1 {
		t.Fatalf("%v %v", ns, err)
	}
	n := ns[0]
	if n.EventID != "1000000000000001" || n.EventType != "payment.succeeded" || n.OccurredAt != 1792283901000 || n.Kind != connector.NotifyPayment {
		t.Errorf("%+v", n)
	}
	p := n.Payment
	if p.Status != connector.PaymentSucceeded || p.Amount != 10050 || p.ProviderRef != "pay_ref_01" || p.ProviderOrderID != "28000000000219002" || p.ProviderPaymentID != "28000000000181002" || p.Method != "upi" {
		t.Errorf("%+v", p)
	}
	if err := connectortestCheck(ns); err != nil {
		t.Error(err)
	}

	rn, err := c.ParseWebhook(ctx, signedWebhookMS(testSecret, string(raw(t, "event_refund_succeeded")), 1792284001000, 1792284001000))
	if err != nil || len(rn) != 1 || rn[0].Refund == nil {
		t.Fatalf("%v %v", rn, err)
	}
	r := rn[0].Refund
	if r.Status != connector.RefundSucceeded || r.Amount != 1000 || r.ProviderRef != "rf_ref_01" || r.ProviderRefundID != "28000000000184001" || r.ProviderPaymentID != "28000000000181002" || rn[0].EventID != "1000000000000002" {
		t.Errorf("%+v", r)
	}

	// Old-style seconds still verify; a stale millisecond timestamp does not.
	if err := c.VerifyWebhook(ctx, signedWebhook(testSecret, body, nowUnix, nowUnix)); err != nil {
		t.Errorf("seconds: %v", err)
	}
	if err := c.VerifyWebhook(ctx, signedWebhookMS(testSecret, body, 1792283901000-3600*1000, 1792283902000)); err == nil {
		t.Error("a stale millisecond timestamp verified")
	}
}

func TestRealWebhookMandateEnvelope(t *testing.T) {
	c := newRFake().conn(t, widgetCfg)
	body := `{"event_id":1000000000000003,"event_id_str":"1000000000000003","event_type":"mandate.active","account_id":"60000000001","live_mode":false,"event_time":1792284001000,` +
		`"event_object":{"mandate":{"mandate_id":"28000000000300001","customer_id":"28000000000200001","amount":"748.50","currency":"INR","amount_rule":"variable","frequency":"monthly","status":"active","payment_method":{"type":"upi","upi":{"upi_id":"buyer@bank"}}}}}`
	ns, err := c.ParseWebhook(context.Background(), signedWebhookMS(testSecret, body, 1792284001000, 1792284001000))
	if err != nil || len(ns) != 1 || ns[0].Mandate == nil {
		t.Fatalf("%v %v", ns, err)
	}
	m := ns[0].Mandate
	if m.ProviderMandateID != "28000000000300001" || m.Status != "active" || m.MaxAmount != 74850 || ns[0].EventID != "1000000000000003" {
		t.Errorf("%+v", m)
	}
}
