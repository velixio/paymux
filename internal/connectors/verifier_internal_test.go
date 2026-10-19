package connectors

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/paymux/paymux/internal/inbound"
	"github.com/paymux/paymux/pkg/connector"
)

// idConn is a connector that may derive a webhook's event id from the request
// alone (connector.WebhookEventIDer); ParseWebhook stands for work that can
// reach the provider, so the inbound handler must not run it.
type idConn struct {
	connector.Connector
	verifyErr error
	parsed    int
	id        string
	idOK      bool
}

func (c *idConn) VerifyWebhook(context.Context, connector.WebhookRequest) error { return c.verifyErr }
func (c *idConn) ParseWebhook(context.Context, connector.WebhookRequest) ([]connector.Notification, error) {
	c.parsed++
	return []connector.Notification{{EventID: "from-parse"}}, nil
}
func (c *idConn) WebhookEventID(connector.WebhookRequest) (string, bool) { return c.id, c.idOK }

// L3: the inbound request must never make Paymux call the provider. A connector
// that can name the event from the request alone is asked for that, and
// ParseWebhook (which may fetch, for an unsigned notice) is left to the
// background job.
func TestVerifierDerivesTheEventIDWithoutParsing(t *testing.T) {
	now := func() time.Time { return time.Unix(1760000000, 0) }
	body := []byte(`{"x":1}`)

	c := &idConn{verifyErr: connector.ErrUnsigned, id: "derived", idOK: true}
	id, err := (&verifier{conn: c, now: now}).Verify(http.Header{}, body)
	if !errors.Is(err, inbound.ErrUnsigned) || id != "derived" || c.parsed != 0 {
		t.Errorf("unsigned: id=%q err=%v parsed=%d, want derived, ErrUnsigned and no parse", id, err, c.parsed)
	}

	c = &idConn{id: "derived", idOK: true}
	if id, err = (&verifier{conn: c, now: now}).Verify(http.Header{}, body); err != nil || id != "derived" || c.parsed != 0 {
		t.Errorf("signed: id=%q err=%v parsed=%d", id, err, c.parsed)
	}

	// A connector that cannot name this event (not a mandate notice, say) is
	// parsed as before.
	c = &idConn{id: "", idOK: false}
	if id, err = (&verifier{conn: c, now: now}).Verify(http.Header{}, body); err != nil || id != "from-parse" || c.parsed != 1 {
		t.Errorf("fallback: id=%q err=%v parsed=%d", id, err, c.parsed)
	}

	// A bad signature is still a hard failure with no id.
	c = &idConn{verifyErr: connector.ErrInvalidSignature, id: "derived", idOK: true}
	if id, err = (&verifier{conn: c, now: now}).Verify(http.Header{}, body); !errors.Is(err, connector.ErrInvalidSignature) || id != "" {
		t.Errorf("invalid: id=%q err=%v", id, err)
	}
}
