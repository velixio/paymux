package cashfree

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/paymux/paymux/pkg/connector"
)

// ParseWebhook runs only on signature-verified bodies, and amounts also come from
// Cashfree API responses: neither should let one value cost seconds of CPU. An exponent in
// an amount goes through big.Rat; time how long a hostile exponent takes.
func TestAudit2AmountExponentCost(t *testing.T) {
	c := newConn(t, http.NotFoundHandler())
	body := `{"type":"PAYMENT_SUCCESS_WEBHOOK","data":{"order":{"order_id":"o1","order_amount":1e999999,"order_currency":"INR"},` +
		`"payment":{"cf_payment_id":1,"payment_status":"SUCCESS",` + strings.Repeat(`"payment_amount":1e999999,`, 100) + `"payment_currency":"INR"}}}`
	start := time.Now()
	_, err := c.ParseWebhook(context.Background(), connector.WebhookRequest{Body: []byte(body)})
	d := time.Since(start)
	t.Logf("ParseWebhook took %v, err=%v, body=%d bytes", d, err, len(body))
	if d > 250*time.Millisecond {
		t.Fatalf("one %d-byte body costs %v of CPU", len(body), d)
	}
}
