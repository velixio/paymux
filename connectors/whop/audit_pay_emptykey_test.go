package whop

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/velixio/paymux/pkg/connector"
	"github.com/velixio/paymux/pkg/connector/connectortest"
)

// AUDIT: a webhook secret of exactly "whsec_" passes config validation
// (non-empty) and decodes to an EMPTY HMAC key, so anyone can sign webhooks.
// Expected: New refuses a secret whose decoded key is empty.
func TestAuditPayWhopSecretThatDecodesToEmptyKeyIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	cfg := json.RawMessage(`{"api_key":"k","company_id":"biz_1","webhook_secret":"whsec_"}`)
	c, err := New(cfg, connectortest.NewDeps(srv, time.Unix(nowUnix, 0)))
	if err != nil {
		return // refused: fine
	}
	body := []byte(`{"id":"evt_1","type":"payment.succeeded","data":{}}`)
	ts := fmt.Sprint(nowUnix)
	m := hmac.New(sha256.New, []byte{}) // the attacker's "secret"
	m.Write([]byte("msg_1." + ts + "."))
	m.Write(body)
	sig := "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
	err = c.VerifyWebhook(context.Background(), connector.WebhookRequest{Body: body, ReceivedAt: nowUnix * 1000,
		Headers: map[string][]string{"webhook-id": {"msg_1"}, "webhook-timestamp": {ts}, "webhook-signature": {sig}}})
	if err == nil {
		t.Fatal("a webhook signed with an empty key was accepted (secret \"whsec_\")")
	}
}
